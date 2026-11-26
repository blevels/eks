#!/bin/bash

set -euo pipefail

# =========================================
# 1. Configuration Constants
# =========================================
CONTAINER_NAME="postgres"
DB_USER="test_user"
DB_NAME="postgres"
INTERVAL=900
export PGPASSWORD="password"
NS_NAME=""
ACCOUNT_NUM=""
EMAIL=""
USERNAME=""
ENC_USERNAME=""
PASSWORD=""
ENC_PASSWORD=""
REGION="us-east-1"
ROLE=""
ENV=""
OAUTH_URL="https://test.test.com/oauth"
FIRECALL_URI="https://test.test.com"
FIRECALL_URL="https://test.test.com/firecall"
FIRECALL_STATUS=""
OPEN_FIRECALL_REQUEST=false
ACCESS_TOKEN=""
DURATION_IN_HRS=8
CM_TICKET=""

# =========================================
# 2. Functions
# =========================================
fetch_namespace_by_id(){
  local ns_id="$1"
  local query_result

  query_result=$(docker exec -e PGPASSWORD="${PGPASSWORD}" "CONTAINER_NAME" \
    psql -U "${DB_USER}" -d "${DB_NAME}" -t -A -F "|"\
    -c "SELECT n.ns, a.ns_account, a.ns_role, a.env FROM eks.namespaces n INNER JOIN eks.accounts a ON n.env = a.env WHERE n.ns_id = '${ns_id}';")

    if [ -z "${query_result}" ]; then
      return 1
    fi

    echo "${query_result}"
    return 0
}

fetch_namespace_by_name(){
  local ns_id="$1"
  local query_result

  query_result=$(docker exec -e PGPASSWORD="${PGPASSWORD}" "CONTAINER_NAME" \
    psql -U "${DB_USER}" -d "${DB_NAME}" -t -A -F "|"\
    -c "SELECT n.ns, a.ns_account, a.ns_role, a.env FROM eks.namespaces n INNER JOIN eks.accounts a ON n.env = a.env WHERE n.ns = '${ns_name}';")

    if [ -z "${query_result}" ]; then
      return 1
    fi

    echo "${query_result}"
    return 0
}

fetch_namespace_all(){
  local ns_id="$1"
  local query_result

  query_result=$(docker exec -e PGPASSWORD="${PGPASSWORD}" "CONTAINER_NAME" \
    psql -U "${DB_USER}" -d "${DB_NAME}" -t -A -F "|"\
    -c "SELECT n.ns, a.ns_account, a.ns_role, a.env FROM eks.namespaces n INNER JOIN eks.accounts a ON n.env = a.env ORDER BY n.ns;")

    if [ -z "${query_result}" ]; then
      return 1
    fi

    echo "${query_result}"
    return 0
}

fetch_access_token() {
  local response

  response=$(curl -sS -L -k -X POST "${OATH_URL}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "client_id=$USERNAME" \
    --data-urlencode "client_secret=$PASSWORD" \
    --data-urlencode "grant_type=client_credentials" \
    --data-urlencode "scope=AppIdClaimsTrust" \
    -w "%{http_code}")

  http_status="${response:${#response}-3}"
  response_body="${response:0:${#response}-3}"

  if [ "${http_status}" -ne 200 ]; then
    echo "Error: Request failed with status code ${http_status}" >&2
    echo "Response: ${response_body}" >&2
    return 1
  fi

  access_token=$(echo "${response_body}" | grep -o '"access_token":"[^"]*' | cut -d'"' -f4)

  if [ -z "${access_token}" ] || [ "${access_token}" == "null" ]; then
    echo "Error: 'access_token' field missing or null in server response." >&2
    return 1
  fi

  ACCESS_TOKEN=${access_token}
}

fetch_firecall_list() {
  local response
  local target_status="active"

  response=$(curl -sS -L "${FIRECALL_URL}" \
    --max-time 15 \
    -H "Accept: application/json" \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -w "%{http_code}")

  http_status="${response:${#response}-3}"
  response_body="${response:0:${#response}-3}"

  if [ "${http_status}" == 200 ]; then
    FIRECALL_STATUS=0
  fi

  echo "$response_body" | jq -r \
  --arg status "$target_status" \
  --arg account "${ACCOUNT_NUM}" \
  '.[] | select(.firecall_status == $status and .account_id == $account) | "\(.requester)|\(.iamRoleArn)|\(.end_time)"' | \
  while IFS="|" read -r item_requester item_role item_end_date; do
    if [] || []; then
      echo "Error: There is no active firecall to connect to the namespace" >&2
    else
      echo "ACTIVE FIRECALL DETAILS:"
      echo "-----------------------------------"
      echo "USER:   $item_requester"
      echo "ROLE:   $item_role"
      echo "END:    $item_end_date"
      echo "-----------------------------------"
    fi
  done
}

connect_eks() {
  echo ""
  echo "Connnecting to the EKS namespace ${NS_NAME}"
  kconnect use eks \
    --username ${USERNAME}
    --password ${PASSWORD}
    --no-version-check \
    --no-input \
    --idp-protocol "saml" \
    --namespace ${NS_NAME} \
    --role-arn "arn:aws:iam::$ACCOUNT_NUM:role/$ROLE" \
    --idp-endpoint "https://test.test.com"
    --idp-provider "PingNTLM"
}

decrypt_input() {
  local encrypted_user="${ENC_USERNAME}"
  local encrypted_pass="${ENC_PASSWORD}"

  if [ -z "encrypted_user" ]; then
    echo "Error: No encrypted string provided to decrypt_password function." >&2
    return 1
  fi

  if [ -z "encrypted_pass" ]; then
    echo "Error: No encrypted string provided to decrypt_password function"
    return 1
  fi

  USERNAME=$(echo "$encrypted_user" | openssl enc -aes-128-cbc -a -d -salt -pass pass:decrypt 2>/dev/null)
  PASSWORD=$(echo "$encrypted_pass" | openssl enc -aes-128-cbc -a -d -salt -pass pass:decrypt 2>/dev/null)

  if [ $? -ne 0 ] || [ -z "USERNAME"]; then
    echo "Error: Decryption failed (bad username, corrupted string, or missing pbkdf2 flag)." >&2
    return 1
  fi

  if [ $? -ne 0 ] || [ -z "PASSWORD"]; then
    echo "Error: Decryption failed (bad username, corrupted string, or missing pbkdf2 flag)." >&2
    return 1
  fi

  EMAIL="$USERNAME@test.com"
}

open_firecall() {
  local response
  local target_status="SUCCESS"

  read -p "Enter a valid production CM # or SNOW INCIDENT #: " cm_ticket_input
  echo ""

  if [ -n "${cm_ticket_input:-}" ]; then
    CM_TICKET=$cm_ticket_input
  fi

  response=$(curl -sS -X POST "${FIRECALL_URI}" \
    -H "Content-Type: application/json" \
    -H "Accept: */*" \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -d "$(
      jq -n \
      --arg role "arn:aws:iam::$ACCOUNT_NUM:role/$ROLE" \
      --arg duration "$DURATION_IN_HRS" \
      --arg ticket "$CM_TICKET" \
      --arg email "$EMAIL" \
      '{
          iam_role_arn: [$role],
          duration_in_hours: $duration,
          ticket: $ticket,
          email: $email
       }')" \
    -w "%{http_code}")

    http_status="${response:${#response}-3}"
    response_body="${response:0:${#response}-3}"

    if [ "${http_status}" ]; then
      FIRECALL_STATUS=0
    fi

  echo "$response_body" | jq -r \
  --arg status "$target_status" \
  '.[] | select(.status == $status) | "\(.firecall_id)|\(.status)|\(.message)"' | \
  while IFS="|" read -r fc_id fc_status fc_message; do
    if [ -z "$fc_id" ] || [ "$fc_id" == "null" ]; then
      echo "Error: The firecall was not opened to connect to the namespace" >&2
    else
      echo "FIRECALL DETAILS:"
      echo "-----------------------------------"
      echo "USER:   $item_requester"
      echo "ROLE:   $item_role"
      echo "END:    $item_end_date"
      echo "-----------------------------------"
    fi
  done
}

check_user() {
  if [ ${ENV} == "prod" ]; then
    echo ""
    echo "WARNING: You have targeted a PRODUCTION environment."
    echo "The following SA account, s${USERNAME}, is being used for this session."
    echo ""

    USERNAME="s${USERNAME}"

    read -s -p "Enter production authorization (CyberArk) password: " PROD_PASSWORD
    echo ""

    if [ -n "${PROD_PASSWORD:-}" ]; then
      PASSWORD=$PROD_PASSWORD
    fi
  fi
}

check_firecall() {
  local firecall_open_input

  if [  ]; then
    echo ""
    echo "WARNING: You have targeted a PRODUCTION environment."
    echo "Checking for an active firecall for the follwing following account, ${USERNAME}."
    echo ""

    fetch_access_token

    fetch_firecall_list

    if [ ${FIRECALL_STATUS} -ne 0 ]; then
      while true; do
        read -p "Would you like to open a firecall request for this environment? (Y/N): " firecall_open_input

        case "" in
          [yY]|[yY][eE][sS])
            OPEN_FIRECALL_REQUEST=true
            break
            ;;
          [nN]|[nN][oO])
            OPEN_FIRECALL_REQUEST=false
            break
            ;;
          *)
            echo "Invalid inpopen_input'. Please enter Y or N." >&2
            echo "------------------------------------------------"
        esac
      done
    fi
  fi
}

# =========================================
# 3. Main Script
# =========================================
INPUT_PARAM="{1:-}"

if [ -z "INPUT_PARAM" ]; then
  echo "Error: Missing namespace identifier."
  echo "Usage: $0 <ns_id>"
  exit 1
fi

if [ "${INPUT_PARAM:-}" = "list" ]; then
  ()echo "NAME|ENV|ID|ACCT|ROLE" && fetch_namespace_all) | column -s '|' -t
  exit 0
fi

PARAM_LENGTH=${#INPUT_PARAM}

if [ "$PARAM_LENGTH" -gt 3 ]; then
  echo "Querying database via namespace for context: ${INPUT_PARAM}..."

  if DB_OUTPUT=$(fetch_namespace_by_name); then
    NS_NAME=$(echo "$DB_OUTPUT" | cut -d '|' -f1)
    ACCOUNT_NUM=$(echo "$DB_OUTPUT" | cut -d '|' -f2)
    ROLE=$(echo "$DB_OUTPUT" | cut -d '|' -f3)
    ENV=$(echo "$DB_OUTPUT" | cut -d '|' -f4)

    echo ""
    echo "NAMESPACE DETAILS:"
    echo "-----------------------------------"
    echo "NAME:      $NS_NAME"
    echo "ACCOUNT:   $ACCOUNT_NUM"
    echo "ROLE:      $ROLE"
    echo "ENV:       $ENV"
    echo "-----------------------------------"

  else
    echo "Error: No data found matching identifier '$INPUT_PARAM'."
    exit 1
  fi
else
  echo "Queryting database via namespace identifier for context: $INPUT_PARAM..."

  if DB_OUTPUT=$(fetch_namespace_by_id "$INPUT_PARAM"); then
    ROW_COUNT=$(echo "$DB_OUTPUT" | wc -l | tr -d ' ')

    if [ "$ROW_COUNT" -eq 1]; then
      SELECTED_ROW="$DB_OUTPUT"
    else
      echo "Multiple matches found for '$INPUT_PARAM' . Please select an option:"
      echo "------------------------------------------------------"

      IFS=$'\n' read -r -d '' -a OPTIONS_ARRAY <<< "$DB_OUTPUT" || true

      PS3="Enter option number (1-$ROW_COUNT): "

      select CHOICE in "${OPTIONS_ARRAY[@]}"; do
        if [ -n "$CHOICE"]; then
          SELECTED_ROW="$CHOICE"
          break
        else
          echo "Invalid selection. Please choose a valid number from the menu."
        fi
      done
      echo "------------------------------------------------------"
    fi

    NS_NAME=$(echo "$SELECTED_ROW" | cut -d '|' -f1)
    ACCOUNT_NUM=$(echo "$SELECTED_ROW" | cut -d '|' -f2)
    ROLE=$(echo "$SELECTED_ROW" | cut -d '|' -f3)
    ENV=$(echo "$SELECTED_ROW" | cut -d '|' -f4)

    echo ""
    echo "NAMESPACE DETAILS:"
    echo "-----------------------------------"
    echo "NAME:      $NS_NAME"
    echo "ACCOUNT:   $ACCOUNT_NUM"
    echo "ROLE:      $ROLE"
    echo "ENV:       $ENV"
    echo "-----------------------------------"

  else
    echo "Error: No data found matching identifier '$INPUT_PARAM'."
    exit 1
  fi
fi

# =========================================
# 4. Update Credentials after interval
# =========================================
while true; do
  # Clear the screen (optional, keeps it clean)
  echo "Last Update: $(date)"

  # Get USERNAME and PASSWORD
  decrypt_input

  # Check environment
  check_user

  # Check if firecall is open
  check_firecall

  # Open firecall if necessary
  if [ "$OPEN_FIRECALL_REQUEST" == true ]; then
    open_firecall
  fi

  # Kconnect command
  connect_eks

  # Countdown timer
  echo "Waiting for next refresh"
  sleep $INTERVAL
done