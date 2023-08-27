# ERDC HPC Scripts
--
Bash scripts to improve workflow on U.S. Army Engineer Research and Development Center's (ERDC) High Performance Computers (HPCs).

> **NOTE:** You should contact the HPC Helpdesk to change your shell to bash.

## Table of Conntents
1. [Installation] (#install)
2. [SSH to new HPC] (#hpc_swap)
3. [Copying files between HPCs] (#rsync)
4. [Quick Portable Bash Script (PBS) File] (#pbs) 


## Installation <a name='install'></a>
To install the HPC scripts, first clone this repository to a directory
of your choice on the HPC

    user@hpc: cd path/of/your/choice
    user@hpc: git clone https://github.com/mayhl/erdc_hpc_scripts.git
    
After cloning the repo, go to your home directory and use the editor of your choice to modify your .bashrc or .bash_profile file.

    user@hpc: cd $HOME
    user@hpc: vim .bashrc
    
To your .bashrc/.bash_profile, add the following lines

    export HPC_CMDS_PATH=path/of/your/choice
    source $HPC_CMDS_PATH/main.sh
    

## SSH to new HPC <a name='hpc_swap'></a>
While connected to an HPC, you can ssh to a new HPC by using the HPC name, e.g., 

    user@jim03: onyx

> **NOTE:** Nested tunnels do not work.

## Copying files between HPCs <a name='rsync'></a>
Files are copied between HPCs via [rsync](https://rsync.samba.org/). Two commands are created for each HPCs: cpName, copy from HPC 'Name' to current HPC; and cp2Name, copy to HPC 'Name' from current HPC. For example:

    user@jim03: cpOnyx /path/onyx/file /path/jim/file
    user@jim03: cp2Onyx /path/onyx/directory /path/jim/directory 


## Quick Portable Bash Script (PBS) File <a name='pbs'></a>
    
Numerical simulations on the HPCs are queued to be executed on an HPC using a PBS file. A PBS file typically has two overall parts:

1. A header containing information describing the resources to be used.
2. As script setting up the numerical simulation and executing it.

When a PBS file is submitted to the HPC, a Job ID is returned to identify the submission.  

#### *\$HOME* and *\$WORKDIR*

On ERDC HPCs, user files are located in the *\$HOME* directory with limited storage (quota). The *\$WORKDIR* is a temporary storage location with no quota, where older files are periodically deleted. Typically, this location is used to store the outputs from numerical simulations.

#### File Structure Mirroring

To simplify the data movement between *\$HOME* and *\$WORKDIR*, the current directory where the job was submitted from (typically the path containing the input/drive files) in *\$HOME* is mirrored to *\$WORKDIR*. In addition, the Job ID is attached as a suffix to the folder where as well as letter identifying the HPC system, e.g.,

    $HOME/path/to/simulation
    
is mirrored to 

    $WORKDIR/path/to/simulation_j12345
    
where 'j' is an abbreviation of Jim and '12345' is the Job ID returned from the PBS submission.

#### Workflow
The workflow for the PBS file generated is as follows:

1. Starting from a directory in the user's *\$HOME* folder containing the simulation's input/driver files.
3. The PBS file is submitted, queuing the script to be executed.
4. The PBS file will copy the input/driver files from *\$HOME*  to the mirror directory in the *\$WORKDIR*.
5. The numerical simulation is output written to the mirrored path in *\$WORKDIR* (or some subdirectory)

#### Quick Generation 

Quick-generation commands can be created for various numerical models. In the config.sh file, new models can be added by modifying the variable, *PBS_MODELS*, e.g.,  

    PBS_MODELS="Fun WW3"

will generate commands for FUNWAVE and WaveWatch3. For each model, two commands will be created: *mkMdlPBS* and *qmkMdlPBS*, where *Mdl* is the model name in *PBS_MODELS*, e.g., 

    mkFunPBS
    
For each model, a parameter specifying the path containing the executable(s) must be added to the config.sh file. For example

    MDL_EXEC_DPATH=/path/to/folder/with/executable

> **NOTE:** The prefix in *EXEC_PATH* will be the capitalization of the model name in *PBS_MODELS*

The *qmkMdlPBS* command is a quicker way to generate a PBS file. In the config.sh file, several new parameters need to be defined:

    MDL_DEFAULT_EXEC='exectuable name'
    MDL_DEFAULT_SUBPROJ='subproject code'
    MLD_DEFAULT_QUEUE='queue'
    MLD_DEFAULT_INPUT='driver/input files'
    MDL_DEFAULT_WALL='simulation (real) wall time'

In addition, *qmkMldPBS*, has optional arugments 

    qmkMldPBS JOB_NAME NUMBER_OF_THREADS FILE1 FILE2 FILE3 ... 
    
where: 

1. *JOB\_NAME* is the name of the job .
2.  *NUMBER\_OF\_THREADS* is the number of threads (not nodes). The command will automatically select the correct number of nodes and MPI threads. 
3. FILE1, FILE2, ... are additional input/driver files to include on top of the file(s) defined in *MDL\_DEFAULT\_INPUT*.

> **NOTE:** *mkMdlPBS* will accept the same parameters; however, will not lead to a complete PBS file. 
 

 

> ### TODO 
1. Create a Makefile to automate installation and create a basic user config file.
2. Add commands to swap between mirrored $HOME relative path and $WORKDIR relative path. 
3. Add quick archive commands 