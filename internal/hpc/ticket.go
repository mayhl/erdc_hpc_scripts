package hpc

import (
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
)

// ticketMargin is how close to expiry a ticket is still trusted: one about to
// lapse mid-transfer is as bad as an expired one, so it triggers a re-pkinit.
const ticketMargin = 5 * time.Minute

// TicketInfo is the parsed state of the local Kerberos credential cache.
type TicketInfo struct {
	Present   bool
	Principal string    // e.g. "alice@REALM.EXAMPLE"
	Expires   time.Time // TGT expiry (zero if unparsed)
}

var (
	reKlistPrincipal = regexp.MustCompile(`Default principal:\s+(\S+)`)
	// The TGT (krbtgt/…) row: "MM/DD/YYYY HH:MM:SS  MM/DD/YYYY HH:MM:SS  krbtgt/…".
	// The second timestamp is the ticket's expiry.
	reKlistTGT = regexp.MustCompile(`(?m)^\s*\d\d/\d\d/\d{4} \d\d:\d\d:\d\d\s+(\d\d/\d\d/\d{4} \d\d:\d\d:\d\d)\s+krbtgt/`)
)

// parseKlist extracts the principal and TGT expiry from MIT klist output. Pure
// (no clock) so it's testable; the caller compares Expires against now.
func parseKlist(out string) TicketInfo {
	var info TicketInfo
	if m := reKlistPrincipal.FindStringSubmatch(out); m != nil {
		info.Principal = m[1]
	}
	if m := reKlistTGT.FindStringSubmatch(out); m != nil {
		if t, err := time.ParseInLocation("01/02/2006 15:04:05", m[1], time.Local); err == nil {
			info.Expires = t
		}
	}
	return info
}

// ticketUsable reports whether info covers user: present, the principal is user
// (with any realm), and the TGT isn't expired or within ticketMargin of it. An
// unparsed expiry is trusted — an odd klist format degrades to the presence check
// rather than forcing a pkinit. Pure (now injected) so it's testable.
func ticketUsable(info TicketInfo, user string, now time.Time) bool {
	if !info.Present {
		return false
	}
	if info.Principal != user && !strings.HasPrefix(info.Principal, user+"@") {
		return false
	}
	if info.Expires.IsZero() {
		return true
	}
	return now.Add(ticketMargin).Before(info.Expires)
}

// TicketOK reports whether a usable ticket for the configured HPC user is already
// cached. Check-only — never runs pkinit, so an unattended caller (the sshfs guard
// under launchd) can't pop a CAC prompt. True when Kerberos isn't in play.
func TicketOK() bool {
	user := config.HPCUser()
	if user == "" {
		return true
	}
	info, hasKlist := Ticket()
	return !hasKlist || ticketUsable(info, user, time.Now())
}

// Ticket runs klist and returns the parsed credential state. The second return is
// false when klist isn't installed (no local Kerberos here). A non-zero klist exit
// means no cached ticket (Present=false).
func Ticket() (TicketInfo, bool) {
	klist, err := exec.LookPath("klist")
	if err != nil {
		return TicketInfo{}, false
	}
	out, err := exec.Command(klist).CombinedOutput()
	if err != nil {
		return TicketInfo{Present: false}, true
	}
	info := parseKlist(string(out))
	info.Present = true
	return info, true
}
