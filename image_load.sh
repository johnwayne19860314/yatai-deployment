#!/bin/bash
#
images=`cat image-list.txt`
if [[ ! -d tgzs ]];then
    mkdir tgzs
fi
for image in $images;do
    if [[ -z $image ]];then
          continue
    fi
    target="${image//\//-}.tar"
    target="${target//:/-}"
    if [[ ! -f tgzs/$target ]];then
        docker save $image -o tgzs/$target
        echo "$image not exists exists"
    fi
done

# ./sync2sealos.py#