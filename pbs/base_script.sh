
# Executable path 
EXEC=${EXEC_DPATH}/${EXEC_NAME}

# Parsing hostname for HPC name
HOSTNAME=$(echo $PBS_O_HOST | cut -d '.' -f 1)
HOSTNAME=$(echo $HOSTNAME | cut -d '-' -f 1)
HOSTNAME=${HOSTNAME//[[:digit:]]/}

# Setting up HPC MPI
if [ "$HOSTNAME" = "onyx" ]; then
        HOST_ID='o'
        EXEC_CMD="aprun -n"
        MOD_CMD="module swap PrgEnv-cray PrgEnv-intel"

elif [ "$HOSTNAME" = "jim" ]; then
        HOST_ID='j'
        EXEC_CMD="mpiexec_mpt -n"
        MOD_CMD=""
else
        echo "----------------------------------------"
        echo "-           Unidentified HPC           -"
        echo "----------------------------------------"
        echo " hostname orig: $PBS_O_HOST"
        echo " hostname filt: $HOSTNAME"
        echo "----------------------------------------"
        exit 1
fi

# Parsing PBS script relative to HOME
RELWORKDIR=${PBS_O_WORKDIR/"${HOME}/"/}

# Parsing job ID 
JOBID=`echo ${PBS_JOBID} | cut -d '.' -f 1`

# Mirroring PBS script path to WORKDIR with 
# suffix of HPC abbreviation and job ID
SIM_DPATH=${WORKDIR}/${OUT_DIR}_${HOSTID}${JOBID}

if [ ! -d ${JOBID} ]; then
  mkdir -p ${SIM_DPATH}
fi

## ---------
## Launching
## ---------

# copy desired/needed files
cd ${PBS_O_WORKDIR}
cp --parents ${INPUT_FILES} ${SIM_DPATH}/
cd ${SIM_DPATH}

${MOD_CMD}
${EXEC_CMD} ${NPROC} ${EXEC}
