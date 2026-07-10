#!/bin/sh
set -eu

apk add --no-cache s3cmd >/dev/null
mkdir -p /data/idx

s3="s3cmd --access_key=sentry --secret_key=sentry --no-ssl --region=us-east-1 --host=seaweedfs:8333 --host-bucket=seaweedfs:8333/%(bucket)"
$s3 ls "s3://${SENTRY_BUCKET}" >/dev/null 2>&1 || $s3 mb "s3://${SENTRY_BUCKET}"

policy="/tmp/${SENTRY_BUCKET}-lifecycle.xml"
printf '%s' "<?xml version=\"1.0\" encoding=\"UTF-8\"?><LifecycleConfiguration><Rule><ID>Sentry-${SENTRY_BUCKET}-Rule</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>${SENTRY_EVENT_RETENTION_DAYS}</Days></Expiration></Rule></LifecycleConfiguration>" > "$policy"
$s3 setlifecycle "$policy" "s3://${SENTRY_BUCKET}"
$s3 getlifecycle "s3://${SENTRY_BUCKET}" >/dev/null
