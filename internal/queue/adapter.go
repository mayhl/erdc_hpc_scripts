package queue

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mayhl/mayhl_utils/internal/shell"
)

// Adapter abstracts a batch scheduler (PBS / SLURM) behind one interface, so the queue
// read-side and `mu job` converge on a common model regardless of which scheduler a
// cluster runs. Each method builds the remote command string (run via hpc.RemoteExec);
// the impls own their binary names, id joining, and quoting — the rules differ per op
// (PBS space-joins everything; SLURM `scancel` space-joins but `scontrol` comma-joins).
type Adapter interface {
	Name() string
	KillCmd(ids []string) string                 // cancel jobs   (qdel / scancel)
	HoldCmd(ids []string, release bool) string   // hold / release (qhold·qrls / scontrol)
	DetailCmd(ids []string) string               // full detail   (qstat -f / scontrol show job)
	ListCmd(all bool, users, self string) string // live queue    (qstat -a / squeue -o …)
	HistCmd(all bool, users, self string) string // finished jobs (qstat -xa / sacct …)
	SubmitCmd(script string, o SubmitOpts) string
	InteractiveCmd(o SubmitOpts, startDir string) string // interactive allocation (qsub -I / salloc) under a tty; startDir (if set) lands the session there, injected per the scheduler's dialect
	Directives(o SubmitOpts) []string                    // header lines (#PBS / #SBATCH) for preview + templates
	ParseDuration(s string) (int, bool)                  // read a scheduler time cell into seconds — DIALECT-SPECIFIC
	ScriptWalltime(script []byte) (string, bool)         // the walltime the script declares FOR THIS dialect
}

// SubmitOpts are the scheduler-neutral submit knobs; mu job sub populates them and the
// adapter maps each to the scheduler's flag. Empty/zero fields fall through to the
// script's own directives / the scheduler default.
type SubmitOpts struct {
	Account string // allocation to charge (-A)
	Queue   string // queue / partition   (-q / -p)
	// QOS is a SLURM-only tier (--qos=). A site may expose a purpose tier — debug,
	// background — as a QOS on the standard partition rather than as a partition of its
	// own, in which case `-p debug` is rejected outright: the name is not a partition.
	// PBS has no equivalent in mu's model — there the same tier IS a queue — so the PBS
	// adapter ignores this field.
	QOS          string
	Walltime     string // HH:MM:SS            (-l walltime= / -t)
	Nodes        int    // node count          (-l select= / -N); 0 = unset
	CoresPerNode int    // PBS select-chunk detail (ncpus/mpiprocs per node); 0 = bare select
	Name         string // job name            (-N / -J)
	// Env are variables the job must see (-v / --export). `mu job tunnel` hands the job its
	// port this way, so the service and the forward cannot disagree about the number.
	Env map[string]string
}

// envPairs renders Env as sorted K=V pairs. Sorted because a map's iteration order is not:
// a submit command that differs between identical runs is one you can't review or diff.
func envPairs(o SubmitOpts) []string {
	if len(o.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+o.Env[k])
	}
	return out
}

// pbsSelect renders the PBS select chunk: nodes alone, or nodes:ncpus:mpiprocs when the
// cores-per-node is known (sites that require a full chunk reject a bare select count).
func pbsSelect(o SubmitOpts) string {
	if o.CoresPerNode > 0 {
		return fmt.Sprintf("%d:ncpus=%d:mpiprocs=%d", o.Nodes, o.CoresPerNode, o.CoresPerNode)
	}
	return strconv.Itoa(o.Nodes)
}

// For returns the adapter for a scheduler name ("pbs" / "slurm"), or nil if unknown —
// callers treat nil the same as the old `switch` default ("" command).
func For(scheduler string) Adapter {
	switch scheduler {
	case "pbs":
		return pbsAdapter{}
	case "slurm":
		return slurmAdapter{}
	default:
		return nil
	}
}

func quoteJoin(ids []string, sep string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = shell.Quote(id)
	}
	return strings.Join(q, sep)
}

// pbsUserSel builds qstat's WHO selector: " -u <users>" for an explicit list, "" for all
// users, else the configured user (self), or "" if unset. Leading-space form to append
// after "qstat -a" / "qstat -xa"; self is the caller-resolved default user (queue stays
// config-free). Shared by the live (ListCmd) and history (HistCmd) PBS commands.
func pbsUserSel(all bool, users, self string) string {
	switch {
	case users != "":
		return " -u " + users
	case all:
		return ""
	default:
		if self != "" {
			return " -u " + self
		}
		return ""
	}
}

// ---- PBS (qsub/qstat/qdel/qhold) ------------------------------------------------------

type pbsAdapter struct{}

// pbsJobRefs trims each id to its leading segment (before the first dot; array
// brackets kept). qstat's listings report ids with a server suffix they TRUNCATE to
// column width — feeding that mangled suffix back to qstat -f/qdel/qhold errors
// with an invalid job id. Every command runs against the cluster's own server,
// where the bare sequence number is unambiguous.
func pbsJobRefs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shortID(id)
	}
	return out
}

func (pbsAdapter) Name() string                { return "pbs" }
func (pbsAdapter) KillCmd(ids []string) string { return "qdel " + quoteJoin(pbsJobRefs(ids), " ") }

func (pbsAdapter) HoldCmd(ids []string, release bool) string {
	bin := "qhold"
	if release {
		bin = "qrls"
	}
	return bin + " " + quoteJoin(pbsJobRefs(ids), " ")
}

func (pbsAdapter) DetailCmd(ids []string) string {
	return "qstat -f " + quoteJoin(pbsJobRefs(ids), " ")
}

func (pbsAdapter) ListCmd(all bool, users, self string) string {
	return "qstat -a" + pbsUserSel(all, users, self)
}

func (pbsAdapter) HistCmd(all bool, users, self string) string {
	return "qstat -xa" + pbsUserSel(all, users, self)
}

// pbsOpts renders the shared qsub option string (submit + interactive) — one flag per
// set SubmitOpts field, leading-space form.
func pbsOpts(o SubmitOpts) string {
	s := ""
	if o.Account != "" {
		s += " -A " + shell.Quote(o.Account)
	}
	if o.Queue != "" {
		s += " -q " + shell.Quote(o.Queue)
	}
	if o.Walltime != "" {
		s += " -l walltime=" + shell.Quote(o.Walltime)
	}
	if o.Nodes > 0 {
		s += " -l select=" + pbsSelect(o)
	}
	if o.Name != "" {
		s += " -N " + shell.Quote(o.Name)
	}
	if e := envPairs(o); len(e) > 0 {
		s += " -v " + shell.Quote(strings.Join(e, ","))
	}
	return s
}

// quoteScriptPath shell-quotes a submit script path but keeps a leading ~ / $HOME EXPANDABLE:
// the path resolves ON the target under `bash -lc`, so `~/run.pbs` has to reach the far shell
// as "$HOME"/run.pbs — a single-quoted literal ~ is a path qsub/sbatch then can't open (the
// footgun that made every ~ / $HOME submit fail until you spelled out the absolute path).
func quoteScriptPath(s string) string {
	switch {
	case s == "~" || s == "$HOME":
		return `"$HOME"`
	case strings.HasPrefix(s, "~/"):
		return `"$HOME"/` + shell.Quote(s[2:])
	case strings.HasPrefix(s, "$HOME/"):
		return `"$HOME"/` + shell.Quote(s[len("$HOME/"):])
	default:
		return shell.Quote(s)
	}
}

func (pbsAdapter) SubmitCmd(script string, o SubmitOpts) string {
	return "qsub" + pbsOpts(o) + " " + quoteScriptPath(script)
}

// InteractiveCmd builds `qsub -I`. startDir can only be honored as a submit-dir prefix: PBS's
// `qsub -I` takes no command to run, so mu cannot inject a `cd` into the interactive shell —
// the cd on the LOGIN node sets PBS_O_WORKDIR=<dir>, but the qsub -I shell still opens in $HOME
// (a `cd $PBS_O_WORKDIR`, e.g. from shellinit, is what actually lands you there).
func (pbsAdapter) InteractiveCmd(o SubmitOpts, startDir string) string {
	cmd := "qsub -I" + pbsOpts(o)
	if startDir != "" {
		return "cd " + shell.Quote(startDir) + " && " + cmd
	}
	return cmd
}

func (pbsAdapter) ParseDuration(s string) (int, bool) { return parsePBSDuration(s) }

func (pbsAdapter) ScriptWalltime(script []byte) (string, bool) {
	return scriptWalltime(rePBSScriptWall, script)
}

// Directives renders the #PBS header lines for preview/templates (display, not exec —
// unquoted). Empty opts yield no lines: the script's own directives / defaults apply.
func (pbsAdapter) Directives(o SubmitOpts) []string {
	var d []string
	if o.Account != "" {
		d = append(d, "#PBS -A "+o.Account)
	}
	if o.Queue != "" {
		d = append(d, "#PBS -q "+o.Queue)
	}
	if o.Walltime != "" {
		d = append(d, "#PBS -l walltime="+o.Walltime)
	}
	if o.Nodes > 0 {
		d = append(d, "#PBS -l select="+pbsSelect(o))
	}
	if o.Name != "" {
		d = append(d, "#PBS -N "+o.Name)
	}
	return d
}

// ---- SLURM (sbatch/squeue/scancel/scontrol) -------------------------------------------

type slurmAdapter struct{}

func (slurmAdapter) Name() string                { return "slurm" }
func (slurmAdapter) KillCmd(ids []string) string { return "scancel " + quoteJoin(ids, " ") }

func (slurmAdapter) HoldCmd(ids []string, release bool) string {
	verb := "hold"
	if release {
		verb = "release"
	}
	return "scontrol " + verb + " " + quoteJoin(ids, ",")
}

func (slurmAdapter) DetailCmd(ids []string) string { return "scontrol show job " + quoteJoin(ids, ",") }

func (slurmAdapter) ListCmd(all bool, users, self string) string {
	sel := "--me " // default: just your jobs
	switch {
	case users != "":
		sel = "-u " + users + " "
	case all:
		sel = ""
	}
	return `squeue -h ` + sel + `-o "%i|%P|%j|%u|%t|%M|%l|%D|%R|%S"`
}

func (slurmAdapter) HistCmd(all bool, users, self string) string {
	sel := "" // sacct has no --me, so "just you" names the configured user explicitly
	switch {
	case users != "":
		sel = "-u " + users + " "
	case all:
		sel = "-a "
	default:
		if self != "" {
			sel = "-u " + self + " "
		}
	}
	return `sacct -X -n -p ` + sel + `-o JobIDRaw,JobName,User,Partition,State,Elapsed,Timelimit,NNodes,Submit,Start,End`
}

// slurmOpts renders the shared sbatch/salloc option string — one flag per set
// SubmitOpts field, leading-space form. Nodes maps to -N (SLURM's node count; the
// select-chunk detail is PBS-only).
func slurmOpts(o SubmitOpts) string {
	s := ""
	if o.Account != "" {
		s += " -A " + shell.Quote(o.Account)
	}
	if o.Queue != "" {
		s += " -p " + shell.Quote(o.Queue)
	}
	if o.QOS != "" {
		s += " --qos=" + shell.Quote(o.QOS)
	}
	if o.Walltime != "" {
		s += " -t " + shell.Quote(o.Walltime)
	}
	if o.Nodes > 0 {
		s += " -N " + strconv.Itoa(o.Nodes)
	}
	if o.Name != "" {
		s += " -J " + shell.Quote(o.Name)
	}
	if e := envPairs(o); len(e) > 0 {
		// ALL first: --export replaces the environment wholesale, and dropping the login env
		// would break every module the site's profile loaded.
		s += " --export=" + shell.Quote("ALL,"+strings.Join(e, ","))
	}
	return s
}

func (slurmAdapter) SubmitCmd(script string, o SubmitOpts) string {
	return "sbatch" + slurmOpts(o) + " " + quoteScriptPath(script)
}

// InteractiveCmd builds `salloc`. When startDir is set, it rides as salloc's COMMAND —
// `salloc … bash -c 'cd <dir> && exec bash -l'` — so the cd runs inside the allocation and the
// interactive shell actually lands in <dir> on the compute node (a login-node prefix cd can't:
// salloc's shell starts in $HOME regardless). bash -l, not $SHELL, to match the site's
// bash-written profile scripts (same reason `mu job shell` uses `bash -lc`).
func (slurmAdapter) InteractiveCmd(o SubmitOpts, startDir string) string {
	cmd := "salloc" + slurmOpts(o)
	if startDir != "" {
		cmd += " bash -c " + shell.Quote("cd "+shell.Quote(startDir)+" && exec bash -l")
	}
	return cmd
}

func (slurmAdapter) ParseDuration(s string) (int, bool) { return parseSLURMDuration(s) }

func (slurmAdapter) ScriptWalltime(script []byte) (string, bool) {
	return scriptWalltime(reSLURMScriptWall, script)
}

// Directives renders the #SBATCH header lines for preview/templates (display, not exec —
// unquoted). Empty opts yield no lines: the script's own directives / defaults apply.
func (slurmAdapter) Directives(o SubmitOpts) []string {
	var d []string
	if o.Account != "" {
		d = append(d, "#SBATCH -A "+o.Account)
	}
	if o.Queue != "" {
		d = append(d, "#SBATCH -p "+o.Queue)
	}
	if o.QOS != "" {
		d = append(d, "#SBATCH --qos="+o.QOS)
	}
	if o.Walltime != "" {
		d = append(d, "#SBATCH -t "+o.Walltime)
	}
	if o.Nodes > 0 {
		d = append(d, "#SBATCH -N "+strconv.Itoa(o.Nodes))
	}
	if o.Name != "" {
		d = append(d, "#SBATCH -J "+o.Name)
	}
	return d
}
