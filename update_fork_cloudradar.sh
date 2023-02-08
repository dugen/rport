#!/usr/bin/env bash


git remote add upstream git@github.com:cloudradar-monitoring/rport.git
git fetch upstream
git checkout master
git merge upstream/master
git push origin master