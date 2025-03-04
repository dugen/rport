#
# Install dependencies
#
dpkg -l|grep -q python3-requests || apt-get -y install python3-requests
# Create the private key that gives access to the private rport-plus repo
test -e ~/.ssh || mkdir ~/.ssh
echo "${RPORT_PLUS_PRIV_KEY}" > ~/.ssh/rport-plus-key
chmod 0400 ~/.ssh/*
#
# Checkout the repo
#
export GIT_SSH_COMMAND="ssh -i ~/.ssh/rport-plus-key"
git clone git@github.com:cloudradar-monitoring/rport-plus.git
cd rport-plus
pwd
git status
#
# Checkout the latest tag
#
PLUS_LATEST=$(git ls-remote --tags origin|tail -n1|awk '{print $2}'|cut -d'/' -f3)
echo "Will checkout rport-plus branch ${PLUS_LATEST}"
git checkout tags/"${PLUS_LATEST}" -b v"${PLUS_LATEST}"
echo "✅ Successfully checked out rport-plus${PLUS_LATEST}"
echo ::set-output name=repo_name::$(basename `git rev-parse --show-toplevel`)
#
# Build the plugin
#
make build
ls -la rport-plus.so
echo "=================================================================="
echo "✅ Successfully built rport-plus.so version ${PLUS_LATEST}"
echo "=================================================================="
#
# Create a tar package
#
export PLUS_ARTIFACT=rport-plus_${PLUS_LATEST}@${GITHUB_REF_NAME}_Linux_$(uname -m).tar.gz
echo "rport-plus v${PLUS_LATEST}; compiled for rportd ${GITHUB_REF_NAME}; built on $(date)" > version.txt
echo "Will create ${PLUS_ARTIFACT} now"
tar czf ${PLUS_ARTIFACT} README.md license.txt version.txt rport-plus.so
tar tzf ${PLUS_ARTIFACT}
ls -la ${PLUS_ARTIFACT}
echo "✅ Successfully created artifact ${PLUS_ARTIFACT}"
#
# Get the release id of the rport (main repo) tag
#
RELEASE_ID=$(../.github/scripts/gh-release-id.py ${GITHUB_REF_NAME})
echo "🚩 RELEASE_ID=$RELEASE_ID"
#
# Upload a file to the existing release assets
#
echo "🚚 Will upload a new asset to the existing release"
curl -v -s --fail -T ${PLUS_ARTIFACT} \
 -H "Authorization: token ${GITHUB_TOKEN}" -H "Content-Type: $(file -b --mime-type ${PLUS_ARTIFACT})" \
 -H "Accept: application/vnd.github.v3+json" \
 "https://uploads.github.com/repos/cloudradar-monitoring/rport/releases/${RELEASE_ID}/assets?name=$(basename ${PLUS_ARTIFACT})"|tee upload.log|jq
