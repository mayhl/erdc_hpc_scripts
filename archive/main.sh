#!/bin/bash
. ${HPC_CMDS_PATH}/config.sh


_chkarch() {

	N_ARGS=${#@}

	if [ $N_ARGS -ne 1 ]; then
		echo "ERROR: Only one (1) argument is accepted, $N_ARGS argument(s) given!"
		return 1
	fi

	if [[ ! -r $1 ]]; then
		echo "ERROR: Argument '$1' is not a valid path!"
		return 1
	fi

	if [[ -d $1 ]]; then
		echo "ERROR: '$1' is a directory, can not archive!"
		return 1
	fi

	FPATH=$(realpath $1)	
	APATH=$(sh $HPC_CMDS_PATH/archive/get_mirror.sh $FPATH)

	if [ $? -ne 0 ]; then
		echo $APATH
		return 1
	fi


	archive ls $PATH 1> /dev/null 2> /dev/null

	echo $?


	ADPATH=$(dirname $APATH)
	AFNAME=$(basename $APATH)

	archive mkdir -p $ADPATH 2> /dev/null
	
	echo "archive put -C $ADPATH $FPATH"

	
}


qarch() {

	CMD=$(_chkarch $1)

	echo $CMD

}


bqarch() {
	
	qarch $@

}

als() {

	CWD=$(realpath $PWD)

	APATH=$(sh $HPC_CMDS_PATH/archive/get_mirror.sh $CWD)

	if [ $? -ne 0 ]; then
			echo $APATH
			return 1
	fi

	# Figure out how to ignore control color character 
	# from causing misalignment in tput columns 
	# --color=always 
	ssh  -o LogLevel=error gold -f "cd $APATH && ls && exit" | column -c$(tput cols)

}

aqsub() {
	
		echo "NEED TO IMPLEMENT"

}


