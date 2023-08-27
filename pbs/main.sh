#!/bin/bash 

. ${HPC_CMDS_PATH}/config.sh

for MODEL in ${PBS_MODELS}; do 
   # Make empty PBS for model
   alias mk${MODEL}PBS="sh ${HPC_CMDS_PATH}/pbs/make.sh ${MODEL} F $@"
   # Makes PBS script with default values for model 
   alias qmk${MODEL}PBS="sh ${HPC_CMDS_PATH}/pbs/make.sh ${MODEL} T $@"
done 




