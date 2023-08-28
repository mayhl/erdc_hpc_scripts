#!/bin/bash


swap() {

	RTN=$(sh ${HPC_CMDS_PATH}/swap_work_home/get_swap.sh)
	ERR=$?

	# Checking for valid error code
	if [ $ERR -gt 2 ] || [ $ERR -lt 0 ]; then
		echo "ERROR: Unknown error code '$ERR' returned by get_swap.sh!"
		return 
	fi

  # General error from get_swap.sh
	if [ $ERR -eq 2 ]; then
		echo $RTN
		return
	fi 

	# Extracting warning message from path
	if [ $ERR -eq 1 ]; then 
		SWAP_PATH=$(echo $RTN | cut -d ' ' -f 1)
		MSG=$(echo $RTN | cut -d ' ' -f2- )
		echo $MSG
		cd $SWAP_PATH
		return 
	fi 

	SWAP_PATH=$RTN 
	cd $SWAP_PATH  

}
