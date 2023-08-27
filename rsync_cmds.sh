#!/bin/bash

. ${HPC_CMDS_PATH}/config.sh

# Wrapper subroutine for using rsync with ssh 
cpHPCWrapper()
{
   rsync ${HPC_RSYNC_OPTS} -e ssh $1 $2
}

# Wrapper subroutine for copying data from current machine to HPC
cp2HPC()
{
   cpHPCWrapper $2 $1:$3
}

# Wrapper subrtouine for copying data from HPC to current machine
cpHPC()
{
   cpHPCWrapper $1:$2 $3
}

# Removing current HPC from list
HOSTNAME=$(hostname | cut -d '.' -f 1)
HOSTNAME=${HOSTNAME//[[:digit:]]/}
for HPC_HOST in ${HPC_HOSTS}; do

  HOST=$(echo ${HPC_HOST} | tr '[a-z]' '[A-Z]')
  HOST_HPCS=${HOST}_HPCS
  HPC_HOST=${HOST}_HOST

  TMP_HPCS=""
  for HOST_HPC in ${!HOST_HPCS}; do
    if [ "$HOST_HPC" != "${HOSTNAME}" ]; then
      TMP_HPCS="${TMP_HPCS} ${HOST_HPC}"
    fi
  done

  #NOTE: Quotes need for list assignment to work
  declare ${HOST}_HPCS="${TMP_HPCS}"

done

# Generating HPC host names 
HPCS=''
for HPC_HOST in ${HPC_HOSTS} ; do
   HOST=$(echo ${HPC_HOST} | tr '[:lower:]' '[:upper:]')
   HOST_HPCS=${HOST}_HPCS
   HPC_HOST=${HOST}_HOST

   for HOST_HPC in ${!HOST_HPCS}; do
      HPCS=$HOST_HPC" "${HPCS}
      HOST_HPCU=$(echo ${HOST_HPC} | tr '[:lower:]' '[:upper:]')
      HOST_HPCL=$(echo ${HOST_HPC} | tr '[:upper:]' '[:lower:]')
      declare ${HOST_HPCU}_HOST=${HOST_HPCL}.${!HPC_HOST}
   done

done

# Generating aliases for connecting to other HPCs and copying data
for HPC in ${HPCS}; do
   HPCU=$(echo ${HPC} | tr '[a-z]' '[A-Z]')
   HPCL=$(echo ${HPC} | tr '[A-Z]' '[a-z]')
   HPC_HOST=${HPCU}_HOST
   declare ${HPCU}_SSH=${HPC_UNAME}@${!HPC_HOST}
   alias ${HPCL}='ssh "$@"'\${${HPCU}_SSH}
   HPCC=`echo ${HPCL:0:1} | tr  '[a-z]' '[A-Z]'`${HPCL:1}
   alias cp2${HPCC}='cp2HPC '\${${HPCU}_SSH}' "$@"'
   alias cp${HPCC}='cpHPC '\${${HPCU}_SSH}' "$@"'
done
