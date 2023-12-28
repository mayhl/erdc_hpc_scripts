#!/bin/bash

# Replace cd command to check if current directory
# is marked as a sshfs parent directory and run connection
# script if so
function cd {

  SCRIPT=".connect.sh"

  if [ ! -f "$SCRIPT" ]; then
    builtin cd $@
  else
		# ISSUE: Doesn't work for ~/.....
		#        add check for absolute paths?
    if [[ $1 == ".." ]]; then
      builtin cd $1
    else
      bash $SCRIPT $1
      if [ $? -eq 0 ]; then
        builtin cd $1
      fi
    fi
  fi
}
