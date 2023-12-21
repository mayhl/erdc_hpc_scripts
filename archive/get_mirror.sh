#!/bin/bash
. ${HPC_CMDS_PATH}/config.sh

# NOTE: Trailing / at end of path is need to differentiate between base paths
#       with beginnings e.g. path/to/example vs path/to/example2
DEFAULT_ARCHIVE_MAPS="${HOME}/,home/
											${WORKDIR}/,work/"


ARCHIVE_MAPS="${DEFAULT_ARCHIVE_MAPS} ${USER_ARCHIVE_MAPS}"

N_ARGS=${#@}
if [ $N_ARGS -ne 1 ]; then
  echo "ERROR: Only one (1) argument is accepted, $N_ARGS argument(s) given!"
  return 1
fi

if [[ ! -r $1 ]]; then
  echo "ERROR: Argument '$FPATH' is not a valid path!"
  return 1
fi

if [[ -d $1 ]]; then
	# Ensure directory path has a single trailing /
	FPATH=${1%/}/
else
	FPATH=$1
fi

# Searching for mapped archive path exists
IS_FOUND=F
for i in ${ARCHIVE_MAPS}; do IFS=","; set -- $i;
	if [[ "${FPATH}" == "$1"* ]]; then
		ARCHIVE_PATH=${FPATH/$1/$2}
		IS_FOUND=T
		break
	fi
done

# Return error if no archive map found 
if [[ $IS_FOUND == F ]]; then
	echo "ERROR: Directory map to archive server not found for directory '${PWD}'!"
	return 1
fi

# Returning mapped archive path
echo $ARCHIVE_PATH
