#!/bin/bash

. ${HPC_CMDS_PATH}/config.sh
IS_TQDM=$(sh ${HPC_CMDS_PATH}/tar/check_tqdm.sh)

if [ $IS_TQDM == 'T' ]; then


	gtar() {

		if [[ "$1" == *.tar.gz ]]; then
			# Extracting and uncompressing with gzip
			ARCHIVE=$1

			source ${HPC_PY_VENV}/bin/activate
			tar -v -xzf $ARCHIVE -C . | \
				tqdm --desc "Files" --total $(tar -tvf $ARCHIVE | wc -l) \
				> /dev/null
			deactivate

		elif [[ "$1" == *.tar ]]; then
			# Calling tar without compression
			qtar $1
		else

			# Taring and compressing with gzip
			DIR=$1
   		ARCHIVE=${1%/}.tar.gz

   		source ${HPC_PY_VENV}/bin/activate
   		SIZE="$(du -sb ${DIR} | cut -f1)"
   		tar -cf - ${DIR} \
    		| tqdm --bytes --total "${SIZE}" --desc "Processing" | gzip \
     		| tqdm --bytes --total "${SIZE}" --desc "Compressed" --position 1 \
				> ${ARCHIVE}
   		deactivate

		fi
	}

	qtar() {

		if [[ "$1" == *.tar.gz ]]; then
			# Calling tar with compression
			gtar $1
		elif [[ "$1" == *.tar ]]; then
			# Extracting files without compression
			ARCHIVE=$1

			source ${HPC_PY_VENV}/bin/activate
			tar -v -xf $ARCHIVE -C . | \
				tqdm --desc "Files" --total $(tar -tvf $ARCHIVE | wc -l) \
				> /dev/null	
   		deactivate

		else
			# Taring without compression
  		DIR=$1
   		ARCHIVE=${1%/}.tar

			source ${HPC_PY_VENV}/bin/activate
   		SIZE=$(du -sb ${DIR} | cut -f1)
   		tar -cf - ${DIR} | tqdm --bytes --total ${SIZE} > ${ARCHIVE}
   		deactivate
		fi
	}
else

	gtar() {

		if [[ "$1" == *.tar.gz ]]; then
			tar -vxf $1
		elif [[ "$1" == *.tar ]]; then
			qtar $1
		else
			# Taring with compression
			DIR=$1
   		ARCHIVE=${1%/}.tar.gz

			tar -czvf $ARCHIVE $DIR 
		fi
	}

	qtar() {

		if [[ "$1" == *.tar.gz ]]; then
			gtar $1
		elif [[ "$1" == *.tar ]]; then
			tar -xvf $1
		else	
			DIR=$1
   		ARCHIVE=${1%/}.tar
	
			tar -cvf $ARCHIVE $DIR 
		fi
	}

fi

bqtar() {

	LOG=${1%/}.qtar.log
	qtar $1 > $LOG 2>&1 &
	renice -n 20 -p $!

}


bgtar() {

	LOG=${1%/}.gtar.log
	gtar $1 > $LOG 2>&1 &
	renice -n 20 -p $!

}


