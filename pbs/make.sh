#!/bin/bash

. ${HPC_CMDS_PATH}/config.sh

# Numerical model to base values off 
MODEL=$(echo $1 | tr '[a-z]' '[A-Z]')
# Switch to fill PBS script with model default values 
IS_DEFAULT=$2
# (Optional) Command line input for PBS name 
JOB_NAME=$3
# (Optional) Number of threads
NTHREADS=$4
# (Optional)
ADD_FILES=${@:4:${#@}}


# Name of PBS script to create 
PBS_NAME="run_script.pbs"

# Path to PBS script code 
PBS_SCRIPT="${HPC_CMDS_PATH}/pbs/base_script.sh"

# Setting up default values is quick create mode
if [ "${IS_DEFAULT}" == "T" ]; then
   DEFAULT_EXEC=$(eval echo \${${MODEL}_DEFAULT_EXEC})
   DEFAULT_SUBPROJ=$(eval echo \${${MODEL}_DEFAULT_SUBPROJ})
   DEFAULT_QUEUE=$(eval echo \${${MODEL}_DEFAULT_QUEUE})
   DEFAULT_INPUT=$(eval echo \${${MODEL}_DEFAULT_INPUT})
   DEFAULT_WALL=$(eval echo \${${MODEL}_DEFAULT_WALL})
fi

# Parsing hostname for HPC name
HOSTNAME=$(hostname | cut -d '.' -f 1)
HOSTNAME=${HOSTNAME//[[:digit:]]/}

# Setting up HPC MPI
if [ "$HOSTNAME" = "onyx" ]; then
	NPROC=44
elif [ "$HOSTNAME" = "jim" ]; then
	NPROC=36
else
        echo "----------------------------------------"
        echo "-           Unidentified HPC           -"
        echo "----------------------------------------"
        echo " hostname orig: $(hostname)"
        echo " hostname filt: $HOSTNAME"
        echo "----------------------------------------"
        exit 1
fi

if [ -z $NTHREADS ]; then
	NNODES=1
else
	if (( $NTHREADS <= ${NPROC} )); then
		NMPI=$NTHREADS
		NNODES=1
	else
		NMPI=$NPROC
		NNODES=$(( ($NTHREADS + ($NPROC - 1) ) / $NPROC))
	fi		
fi


# Model specfic exectuable directory path 
EXEC_DPATH=$(eval echo \${${MODEL}_EXEC_DPATH})

# NOTE: $'\n' is used for newline char with heredoc syntax,
#       e.g. cat << EOF > file.txt

INPUT_FILES="${DEFAULT_INPUT} ${ADD_FILES}"
FMT_INPUT_FILES=""
for INPUT_FILE in ${INPUT_FILES}; do 
   FMT_INPUT_FILES="${FMT_INPUT_FILES}${INPUT_FILE}"$'\n'"             "
done

# Removing last newline char and spacing
# NOTE: Should only trigger if DEFAULT_INPUT is not empty 
N=${#FMT_INPUT_FILES}
[[ $N -gt 14 ]] && FMT_INPUT_FILES=${FMT_INPUT_FILES::-14}

# Create PBS header and user input options
cat << EOF > $PBS_NAME
#!/bin/bash
## ----------------------
## Required PBS Directive
## ----------------------
#PBS -A $DEFAULT_SUBPROJ
#PBS -q $DEFAULT_QUEUE
#PBS -N $JOB_NAME
#PBS -l select=$NNODES:ncpus=$NPROC:mpiprocs=$NMPI
#PBS -l walltime=$DEFAULT_WALL
#PBS -l application=other
#PBS -j oe
## PBS -M $PBS_EMAIL
## PBS -m be

## -------------------------------------------
##              BEGIN USER INPUT          
## -------------------------------------------

# Path to executable directory
EXEC_DPATH=\${HOME}/$EXEC_DPATH

# Name of executable
EXEC_NAME=$DEFAULT_EXEC

# Number of processors - must match mpiprocs
NPROC=$NTHREADS

# List input files for simulation
INPUT_FILES='$FMT_INPUT_FILES'

## -------------------------------------------
##               END USER INPUT           
## -------------------------------------------
EOF

# Concatenating PBS script 
cat $PBS_SCRIPT >> $PBS_NAME
