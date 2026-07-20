package queue

import (
	"strings"
	"testing"
)

func TestAdapterFor(t *testing.T) {
	if For("pbs").Name() != "pbs" || For("slurm").Name() != "slurm" {
		t.Fatal("For() returned the wrong adapter")
	}
	if For("bogus") != nil {
		t.Error("unknown scheduler should be nil")
	}
}

func TestAdapterCmds(t *testing.T) {
	// PBS ids normalize to the leading segment (listings truncate the server suffix,
	// which qdel/qstat -f then reject); array brackets must stay, and stay quoted.
	pbsIDs := []string{"1284570.hpc1.examp", "1284[7].hpc1"}
	slurmIDs := []string{"9001", "9002"}
	cases := []struct{ name, got, want string }{
		{"pbs kill", For("pbs").KillCmd(pbsIDs), `qdel '1284570' '1284[7]'`},
		{"slurm kill", For("slurm").KillCmd(slurmIDs), `scancel '9001' '9002'`},
		{"pbs hold", For("pbs").HoldCmd(pbsIDs, false), `qhold '1284570' '1284[7]'`},
		{"pbs rls", For("pbs").HoldCmd(pbsIDs, true), `qrls '1284570' '1284[7]'`},
		{"slurm hold", For("slurm").HoldCmd(slurmIDs, false), `scontrol hold '9001','9002'`},
		{"slurm rls", For("slurm").HoldCmd([]string{"9001"}, true), `scontrol release '9001'`},
		{"pbs detail", For("pbs").DetailCmd(pbsIDs), `qstat -f '1284570' '1284[7]'`},
		{"slurm detail", For("slurm").DetailCmd(slurmIDs), `scontrol show job '9001','9002'`},
		{"pbs submit", For("pbs").SubmitCmd("run.pbs", SubmitOpts{Account: "PROJ1", Queue: "standard"}), `qsub -A 'PROJ1' -q 'standard' 'run.pbs'`},
		{"slurm submit", For("slurm").SubmitCmd("run.slurm", SubmitOpts{Account: "PROJ1"}), `sbatch -A 'PROJ1' 'run.slurm'`},
		{"pbs submit bare", For("pbs").SubmitCmd("run.pbs", SubmitOpts{}), `qsub 'run.pbs'`},
		{"pbs submit tilde", For("pbs").SubmitCmd("~/run.pbs", SubmitOpts{}), `qsub "$HOME"/'run.pbs'`},
		{"slurm submit tilde", For("slurm").SubmitCmd("~/dir/run.slurm", SubmitOpts{}), `sbatch "$HOME"/'dir/run.slurm'`},
		{"pbs submit HOME", For("pbs").SubmitCmd("$HOME/run.pbs", SubmitOpts{}), `qsub "$HOME"/'run.pbs'`},
		{"pbs submit bare tilde", For("pbs").SubmitCmd("~", SubmitOpts{}), `qsub "$HOME"`},
		{"pbs submit absolute", For("pbs").SubmitCmd("/p/home/u/run.pbs", SubmitOpts{}), `qsub '/p/home/u/run.pbs'`},
		{
			"pbs submit full",
			For("pbs").SubmitCmd("run.pbs", SubmitOpts{Account: "PROJ1", Queue: "standard", Walltime: "12:00:00", Nodes: 2, CoresPerNode: 128, Name: "wave"}),
			`qsub -A 'PROJ1' -q 'standard' -l walltime='12:00:00' -l select=2:ncpus=128:mpiprocs=128 -N 'wave' 'run.pbs'`,
		},
		{
			"pbs submit bare select", // no cores-per-node config → bare node count
			For("pbs").SubmitCmd("run.pbs", SubmitOpts{Nodes: 4}),
			`qsub -l select=4 'run.pbs'`,
		},
		{
			"slurm submit full",
			For("slurm").SubmitCmd("run.slurm", SubmitOpts{Account: "PROJ1", Queue: "debug", Walltime: "0:30:00", Nodes: 2, CoresPerNode: 128, Name: "wave"}),
			`sbatch -A 'PROJ1' -p 'debug' -t '0:30:00' -N 2 -J 'wave' 'run.slurm'`,
		},
		{"pbs interactive", For("pbs").InteractiveCmd(SubmitOpts{Queue: "debug", Walltime: "1:00:00"}, ""), `qsub -I -q 'debug' -l walltime='1:00:00'`},
		{"slurm interactive", For("slurm").InteractiveCmd(SubmitOpts{Queue: "debug", Nodes: 1}, ""), `salloc -p 'debug' -N 1`},
		// --dir: SLURM rides the cd as salloc's command (lands the compute shell); PBS can only
		// prefix a submit-dir cd (qsub -I takes no command).
		{"slurm interactive dir", For("slurm").InteractiveCmd(SubmitOpts{Queue: "debug", Nodes: 1}, "/work/proj"), `salloc -p 'debug' -N 1 bash -c 'cd '\''/work/proj'\'' && exec bash -l'`},
		{"pbs interactive dir", For("pbs").InteractiveCmd(SubmitOpts{Queue: "debug"}, "/work/proj"), `cd '/work/proj' && qsub -I -q 'debug'`},
		{"pbs list you", For("pbs").ListCmd(false, "", "alice"), "qstat -a -u alice"},
		{"pbs list all", For("pbs").ListCmd(true, "", "alice"), "qstat -a"},
		{"pbs list users", For("pbs").ListCmd(false, "bob,carol", "alice"), "qstat -a -u bob,carol"},
		{"slurm list you", For("slurm").ListCmd(false, "", "alice"), `squeue -h --me -o "%i|%P|%j|%u|%t|%M|%l|%D|%R|%S"`},
		{"slurm list all", For("slurm").ListCmd(true, "", "alice"), `squeue -h -o "%i|%P|%j|%u|%t|%M|%l|%D|%R|%S"`},
		{"pbs hist you", For("pbs").HistCmd(false, "", "alice"), "qstat -xa -u alice"},
		{"slurm hist you", For("slurm").HistCmd(false, "", "alice"), `sacct -X -n -p -u alice -o JobIDRaw,JobName,User,Partition,State,Elapsed,Timelimit,NNodes,Submit,Start,End`},
		{"slurm hist all", For("slurm").HistCmd(true, "", ""), `sacct -X -n -p -a -o JobIDRaw,JobName,User,Partition,State,Elapsed,Timelimit,NNodes,Submit,Start,End`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, c.got, c.want)
		}
	}
}

func TestAdapterDirectives(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"pbs both", strings.Join(For("pbs").Directives(SubmitOpts{Account: "PROJ1", Queue: "standard"}), "\n"), "#PBS -A PROJ1\n#PBS -q standard"},
		{"slurm both", strings.Join(For("slurm").Directives(SubmitOpts{Account: "PROJ1", Queue: "debug"}), "\n"), "#SBATCH -A PROJ1\n#SBATCH -p debug"},
		{"pbs bare", strings.Join(For("pbs").Directives(SubmitOpts{}), "\n"), ""},
		{"slurm account only", strings.Join(For("slurm").Directives(SubmitOpts{Account: "PROJ1"}), "\n"), "#SBATCH -A PROJ1"},
		{
			"pbs full",
			strings.Join(For("pbs").Directives(SubmitOpts{Walltime: "12:00:00", Nodes: 2, CoresPerNode: 64, Name: "wave"}), "\n"),
			"#PBS -l walltime=12:00:00\n#PBS -l select=2:ncpus=64:mpiprocs=64\n#PBS -N wave",
		},
		{
			"slurm full",
			strings.Join(For("slurm").Directives(SubmitOpts{Walltime: "0:30:00", Nodes: 2, Name: "wave"}), "\n"),
			"#SBATCH -t 0:30:00\n#SBATCH -N 2\n#SBATCH -J wave",
		},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, c.got, c.want)
		}
	}
}
