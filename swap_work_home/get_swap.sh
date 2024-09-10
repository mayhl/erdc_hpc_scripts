#!/bin/bash

. ${HPC_CMDS_PATH}/config.sh
# Script to echo the mirror path in either $HOME or $WORK
# 	Returns exit code 0 and echos mirrored path if valid path found
# 	Returns exit code 1 and echos alternative mirrored path followed by warning message 
# 	Returns exit code 2 and echos error message if valid mirrored path is not found


# Checking if either in $WORKDIR or $HOME
# FUTURE: Better fix for trailing / and paths with shared bases
#         e.g. path/example vs path/example_2
PWD=$(realpath $(pwd))/

IS_FOUND='F'
for i in "${!HPC_SWAP_HOMEDIRS[@]}"; do

	HDIR=${HPC_SWAP_HOMEDIRS[i]}
	WDIR=${HPC_SWAP_WORKDIRS[i]}
	if [[ $PWD == $HDIR/* ]]; then
		SWAP_PATH=${PWD/$HDIR/$WDIR}
		IS_FOUND='T'
		IS_HOME='T'
		break
	elif [[ $PWD == $WDIR/* ]]; then
		SWAP_PATH=${PWD/$WDIR/$HDIR}
		IS_FOUND='T'
		IS_HOME='F'
		break
	fi

done

if [[ $IS_FOUND == 'F' ]]; then
	echo "ERROR: Can not swap, current path is neither in ${HPC_SWAP_HOMEDIRS[@]} or in ${HPC_SWAP_WORKDIRS[@]}!"
	exit 2
fi


# Simple mirrored path found
if [[ -d $SWAP_PATH ]]; then
	echo $SWAP_PATH
	exit 0
fi 

# Removing trailing _XXXX suffix from path, e.g., Job ID for simulations in $WORKDIR
ALT_SWAP_PATH=$(echo $SWAP_PATH | rev | cut -d '_' -f2- | rev)

if [[ $IS_HOME == 'F' ]]; then
	# Simple swap if going from $WORKDIR to $HOME
	if [[ -d $ALT_SWAP_PATH ]]; then
			echo $ALT_SWAP_PATH
			exit 0
	fi
else
	# Checking mirrored simulation runs exists, e.g., contains _XXXX suffix
	# NOTE: 2>/dev/null redirecting error if pattern does not exists 
	N=$(ls -d "${ALT_SWAP_PATH}"_* 2>/dev/null | wc -l) 
	if [[ "$N" > 0 ]]; then
		echo ${ALT_SWAP_PATH/"/$(basename $ALT_SWAP_PATH)"/}
		echo "Warning: Swapping to parent folder containing simulation runs."
		exit 1
	fi
fi

# No mirrored path if script reaches end 
echo "ERROR: No mirrored path found!"
exit 2
