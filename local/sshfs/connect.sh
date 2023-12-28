#! /bin/bash

MPATH=${1%/}
SCRIPT=".${MPATH}.sh"

source spinner.sh

# sshfs options
#OPTS=""

MAPATH=$PWD/$MPATH

if [ ! -d "$MPATH" ]; then
  echo "ERROR: Directory '$MPATH' does not exist!"
	exit 1
fi

if [ ! -f "$SCRIPT" ]; then
  echo "ERROR: Mount script '$SCRIPT' does not exist!"
	exit 1
fi

HPATH=$(sh $SCRIPT)
HPATH=${HPATH/ /:}


MPATHS=$(mount | grep macfuse | cut -d ' ' -f 3)

# Check if path is already mounted with macfuse/sshfs
IS_MOUNTED=F
for TMP_MPATH in $MPATHS; do
	if [[ $MAPATH == $TMP_MPATH ]]; then
		IS_MOUNTED=T
		break
	fi
done 

# Checking if path need to be mounted
if [[ $IS_MOUNTED == "T" ]]; then

	# Check if mount connection is working
	ls $MAPATH 1> /dev/null 2> /dev/null
	if [ $? -eq 0 ]; then
		# Mount connection working
		MOUNT=F
	else
		# Mount connect not working
		echo "WARNING: Reestablishing mount"
		umount -f $MAPATH
		MOUNT=T
	fi
else
	# Not mounted already so mounting
	MOUNT=T
fi

if [[ $MOUNT == 'T' ]]; then

	echo -n "Connecting "

	sshfs $HPATH $MAPATH & spinner $! #"${OPTS[@]}"

	echo ""
	if [ $? -eq 0 ]; then
		echo "Mount connection established."
	else
		echo "ERROR: Failed to establish mount connection!"
	fi

	


fi
