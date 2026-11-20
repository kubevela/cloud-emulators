#!/usr/bin/env bash
# Check an endpoint the way an ACK DynamoDB controller would use it: plain HTTP, access
# keys the emulator never issued, and the calls the controller makes on a Table.
#
#   aws/smoke-test.sh [ENDPOINT]        default http://127.0.0.1:4566
set -euo pipefail

ENDPOINT=${1:-http://127.0.0.1:4566}
export AWS_ACCESS_KEY_ID=000000000000 AWS_SECRET_ACCESS_KEY=not-a-real-key
export AWS_REGION=us-west-2 AWS_DEFAULT_REGION=us-west-2 AWS_PAGER="" AWS_EC2_METADATA_DISABLED=true
unset AWS_PROFILE AWS_SESSION_TOKEN

for _ in $(seq 60); do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' "$ENDPOINT/__admin/health" || true)" == 200 ]] && break
  sleep 1
done
[[ "$(curl -s "$ENDPOINT/__admin/health")" == ok ]] || { echo "FAIL: $ENDPOINT/__admin/health did not answer ok" >&2; exit 1; }

table=smoke-$RANDOM
ddb() { aws --endpoint-url "$ENDPOINT" dynamodb "$@"; }
check() {  # LABEL EXPECTED ACTUAL
  if [[ "$3" == "$2" ]]; then echo "ok    $1: $3"; else echo "FAIL  $1: expected '$2', got '$3'" >&2; exit 1; fi
}

check "account" 000000000000 "$(aws --endpoint-url "$ENDPOINT" sts get-caller-identity --query Account --output text)"
ddb create-table --table-name "$table" \
  --attribute-definitions AttributeName=id,AttributeType=S --key-schema AttributeName=id,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST --tags Key=team,Value=platform >/dev/null
ddb wait table-exists --table-name "$table"
check "table status" ACTIVE "$(ddb describe-table --table-name "$table" --query Table.TableStatus --output text)"
check "table arn" "arn:aws:dynamodb:us-west-2:000000000000:table/$table" "$(ddb describe-table --table-name "$table" --query Table.TableArn --output text)"
check "time to live" DISABLED "$(ddb describe-time-to-live --table-name "$table" --query TimeToLiveDescription.TimeToLiveStatus --output text)"
check "contributor insights" DISABLED "$(ddb describe-contributor-insights --table-name "$table" --query ContributorInsightsStatus --output text)"
check "tag" platform "$(ddb list-tags-of-resource --resource-arn "arn:aws:dynamodb:us-west-2:000000000000:table/$table" --query 'Tags[0].Value' --output text)"
# A request AWS refuses must be refused the same way, with AWS's own error code.
err=$(ddb create-table --table-name x --attribute-definitions AttributeName=id,AttributeType=S \
  --key-schema AttributeName=id,KeyType=HASH --billing-mode PAY_PER_REQUEST 2>&1 || true)
[[ "$err" == *ValidationException* ]] && echo "ok    bad request: ValidationException" || { echo "FAIL  bad request: $err" >&2; exit 1; }
ddb delete-table --table-name "$table" >/dev/null
echo "smoke test passed against $ENDPOINT"
