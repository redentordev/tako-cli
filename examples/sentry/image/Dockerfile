ARG SENTRY_IMAGE
FROM ${SENTRY_IMAGE}

# Matches getsentry/self-hosted 26.6.0's sentry/Dockerfile, while pinning the
# nodestore commit that main referenced at that release instead of rebuilding
# mutable main. Every consumer uses this one shared image artifact.
RUN pip install --no-cache-dir https://github.com/getsentry/sentry-nodestore-s3/archive/a1457f93d266485d101ae3b29fa03028bb3d63c2.zip
