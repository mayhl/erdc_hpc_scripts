package cli

import "testing"

func TestHoldCmd(t *testing.T) {
	if got := holdCmd("pbs", false, []string{"1284570.hpc1", "1284571.hpc1"}); got != `qhold '1284570' '1284571'` {
		t.Errorf("pbs hold: %q", got)
	}
	if got := holdCmd("pbs", true, []string{"1284570.hpc1"}); got != `qrls '1284570'` {
		t.Errorf("pbs release: %q", got)
	}
	// SLURM uses scontrol hold/release with a comma list.
	if got := holdCmd("slurm", false, []string{"9001", "9002"}); got != `scontrol hold '9001','9002'` {
		t.Errorf("slurm hold: %q", got)
	}
	if got := holdCmd("slurm", true, []string{"9001"}); got != `scontrol release '9001'` {
		t.Errorf("slurm release: %q", got)
	}
	if got := holdCmd("", false, []string{"1"}); got != "" {
		t.Errorf("unknown scheduler: %q", got)
	}
}
