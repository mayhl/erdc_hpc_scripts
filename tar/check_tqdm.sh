#!/bin/bash
. ${HPC_CMDS_PATH}/config.sh

if [ -d $HPC_PY_VENV ]; then
	SRC_FILE=$HPC_PY_VENV/bin/activate
	if [ -f $SRC_FILE ]; then
		source $SRC_FILE
	fi
fi

$HPC_PYTHON -c "import tqdm" 2> /dev/null

if [ $? -eq 0 ]; then echo 'T'; else echo 'F'; fi
