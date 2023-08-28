#!/bin/bash

# Script to echo the mirror path in either $HOME or $WORK
# 	Returns exit code 0 and echos mirrored path if valid path found
# 	Returns exit code 1 and echos alternative mirrored path followed by warning message 
# 	Returns exit code 2 and echos error message if valid mirrored path is not found


# Checking if either in $WORKDIR or $HOME
PWD=$(pwd)
if [[ $PWD == $HOME* ]]; then
	IS_HOME='T'
	SWAP_PATH=${PWD/$HOME/$WORKDIR}
elif [[ $PWD == $WORKDIR* ]]; then
	IS_HOME='F'
	SWAP_PATH=${PWD/$WORKDIR/$HOME}
else
	echo "ERROR: Can now swap, current path is neither in \$HOME or \$WORKDIR!"
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
