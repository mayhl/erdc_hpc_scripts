package cli

import "testing"

func TestDetailCmd(t *testing.T) {
	if got := detailCmd("pbs", []string{"1284570.hpc1", "1284571.hpc1"}); got != `qstat -f '1284570' '1284571'` {
		t.Errorf("pbs: %q", got)
	}
	// SLURM scontrol takes a comma list, not space-separated args.
	if got := detailCmd("slurm", []string{"9001", "9002"}); got != `scontrol show job '9001','9002'` {
		t.Errorf("slurm: %q", got)
	}
	// PBS array brackets stay quoted so the remote shell doesn't glob them.
	if got := detailCmd("pbs", []string{"1284[7].hpc1"}); got != `qstat -f '1284[7]'` {
		t.Errorf("array id: %q", got)
	}
	if got := detailCmd("", []string{"1"}); got != "" {
		t.Errorf("unknown scheduler: %q", got)
	}
}
