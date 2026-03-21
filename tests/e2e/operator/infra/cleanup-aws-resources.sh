#!/bin/bash
# cleanup-aws-resources.sh
# Cleanup all AWS resources created by e2e tests
#
# Usage:
#   ./cleanup-aws-resources.sh [--dry-run] <TEST_RUN_ID>  # Safe - deletes only specific test run
#   ./cleanup-aws-resources.sh [--dry-run] --all          # Dangerous - deletes ALL e2e resources
#   ./cleanup-aws-resources.sh [--dry-run] --before-date <MM/DD/YYYY> --all  # Delete old zombie resources
#
# Examples:
#   ./cleanup-aws-resources.sh abc123-1710691200          # Delete resources from specific test
#   ./cleanup-aws-resources.sh --dry-run abc123-1710691200 # Show what would be deleted (dry run)
#   ./cleanup-aws-resources.sh --all                      # Delete all e2e test resources (use with caution!)
#   ./cleanup-aws-resources.sh --dry-run --all            # Show all resources that would be deleted
#   ./cleanup-aws-resources.sh --before-date 03/18/2026 --all  # Delete resources created before 03/18/2026
#   ./cleanup-aws-resources.sh --dry-run --before-date 03/18/2026 --all  # Show old resources

set -e

REGION="${AWS_REGION:-us-east-1}"
TAG_KEY="ManagedBy"
TAG_VALUE="helm-charts-e2e"
DRY_RUN=false
DELETE_ALL=false
TEST_RUN_ID=""
CLUSTER_NAME=""
BEFORE_DATE=""
BEFORE_TIMESTAMP=0

# Parse arguments - allow flags in any order
if [ $# -eq 0 ]; then
    echo "Error: Missing required argument"
    echo ""
    echo "Usage:"
    echo "  $0 [--dry-run] <TEST_RUN_ID>                      # Delete resources from specific test run (SAFE)"
    echo "  $0 [--dry-run] --cluster <CLUSTER_NAME>           # Delete specific cluster only (SAFE)"
    echo "  $0 [--dry-run] --all                              # Delete ALL e2e resources (DANGEROUS)"
    echo "  $0 [--dry-run] --before-date <MM/DD/YYYY> --all   # Delete resources created before date"
    echo ""
    echo "Examples:"
    echo "  $0 abc123-1710691200"
    echo "  $0 --dry-run abc123-1710691200"
    echo "  $0 --cluster aws-chart-testing-cluster-0-abc123"
    echo "  $0 --dry-run --cluster aws-chart-testing-cluster-0-abc123"
    echo "  $0 --all"
    echo "  $0 --dry-run --all"
    echo "  $0 --before-date 03/18/2026 --all"
    echo "  $0 --dry-run --before-date 03/18/2026 --all"
    exit 1
fi

# Parse all arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --all)
            DELETE_ALL=true
            shift
            ;;
        --cluster)
            if [ -z "$2" ]; then
                echo "Error: --cluster requires a cluster name"
                exit 1
            fi
            CLUSTER_NAME="$2"
            shift 2
            ;;
        --before-date)
            if [ -z "$2" ]; then
                echo "Error: --before-date requires a date in MM/DD/YYYY format"
                exit 1
            fi
            BEFORE_DATE="$2"
            # Convert date to Unix timestamp using date command
            # macOS date command format: date -j -f "%m/%d/%Y" "03/18/2026" "+%s"
            # Linux date command format: date -d "03/18/2026" "+%s"
            if date -j -f "%m/%d/%Y" "$BEFORE_DATE" "+%s" >/dev/null 2>&1; then
                # macOS
                BEFORE_TIMESTAMP=$(date -j -f "%m/%d/%Y" "$BEFORE_DATE" "+%s")
            elif date -d "$BEFORE_DATE" "+%s" >/dev/null 2>&1; then
                # Linux
                BEFORE_TIMESTAMP=$(date -d "$BEFORE_DATE" "+%s")
            else
                echo "Error: Invalid date format '$BEFORE_DATE'. Use MM/DD/YYYY format (e.g., 03/18/2026)"
                exit 1
            fi
            shift 2
            ;;
        *)
            if [ -z "$TEST_RUN_ID" ] && [ -z "$CLUSTER_NAME" ]; then
                TEST_RUN_ID="$1"
            else
                echo "Error: Unexpected argument '$1'"
                exit 1
            fi
            shift
            ;;
    esac
done

# Validate arguments
if [ "$DELETE_ALL" = false ] && [ -z "$TEST_RUN_ID" ] && [ -z "$CLUSTER_NAME" ]; then
    echo "Error: Must specify either --all, --cluster <name>, or a TEST_RUN_ID"
    exit 1
fi

if [ "$DELETE_ALL" = true ] && [ -n "$TEST_RUN_ID" ]; then
    echo "Error: Cannot specify both --all and a TEST_RUN_ID"
    exit 1
fi

if [ "$DELETE_ALL" = true ] && [ -n "$CLUSTER_NAME" ]; then
    echo "Error: Cannot specify both --all and --cluster"
    exit 1
fi

if [ -n "$TEST_RUN_ID" ] && [ -n "$CLUSTER_NAME" ]; then
    echo "Error: Cannot specify both TEST_RUN_ID and --cluster"
    exit 1
fi

if [ -n "$BEFORE_DATE" ] && [ "$DELETE_ALL" = false ]; then
    echo "Error: --before-date can only be used with --all"
    exit 1
fi

# Show dry-run banner if enabled
if [ "$DRY_RUN" = true ]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "🔍 DRY RUN MODE - No resources will be deleted"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
fi

if [ "$DELETE_ALL" = true ]; then
    if [ "$DRY_RUN" = false ]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        if [ -n "$BEFORE_DATE" ]; then
            echo "⚠️  WARNING: DELETING OLD HELM-CHARTS-E2E RESOURCES"
        else
            echo "⚠️  WARNING: DELETING ALL HELM-CHARTS-E2E RESOURCES"
        fi
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo ""
        if [ -n "$BEFORE_DATE" ]; then
            echo "This will delete resources tagged with ManagedBy=helm-charts-e2e"
            echo "in region $REGION that were created before $BEFORE_DATE."
        else
            echo "This will delete ALL resources tagged with ManagedBy=helm-charts-e2e"
            echo "in region $REGION, regardless of which test created them."
        fi
        echo ""
        echo "⚠️  This WILL interfere with any concurrent tests!"
        echo ""
        echo "Type 'DELETE ALL' (in capitals) to confirm, or Ctrl+C to cancel:"
        read -r confirmation

        if [ "$confirmation" != "DELETE ALL" ]; then
            echo "Aborted - confirmation text did not match"
            exit 1
        fi

        if [ -n "$BEFORE_DATE" ]; then
            echo "Proceeding with deletion of resources created before $BEFORE_DATE..."
        else
            echo "Proceeding with deletion of ALL resources..."
        fi
    else
        if [ -n "$BEFORE_DATE" ]; then
            echo "Showing resources created before $BEFORE_DATE (ManagedBy=helm-charts-e2e) in region $REGION"
        else
            echo "Showing ALL resources that would be deleted (ManagedBy=helm-charts-e2e) in region $REGION"
        fi
    fi
elif [ -n "$CLUSTER_NAME" ]; then
    if [ "$DRY_RUN" = false ]; then
        echo "Cleaning up cluster: $CLUSTER_NAME in region $REGION"
    else
        echo "Showing what would be deleted for cluster: $CLUSTER_NAME in region $REGION"
    fi
else
    if [ "$DRY_RUN" = false ]; then
        echo "Cleaning up AWS resources for Test Run ID: $TEST_RUN_ID in region $REGION"
    else
        echo "Showing resources that would be deleted for Test Run ID: $TEST_RUN_ID in region $REGION"
    fi
fi

# Function to get TestRunID from tags
get_test_run_id() {
    local tags_json="$1"
    if [ -z "$tags_json" ] || [ "$tags_json" = "[]" ] || [ "$tags_json" = "{}" ]; then
        echo ""
        return
    fi

    # Handle both array format (EC2 resources) and object format (EKS resources)
    local test_run_id=""
    if echo "$tags_json" | jq -e 'type == "array"' >/dev/null 2>&1; then
        test_run_id=$(echo "$tags_json" | jq -r '.[] | select(.Key=="TestRunID") | .Value' 2>/dev/null || echo "")
    else
        test_run_id=$(echo "$tags_json" | jq -r '.TestRunID // ""' 2>/dev/null || echo "")
    fi

    echo "$test_run_id"
}

# Function to extract timestamp from TestRunID
# TestRunID format: "abc123-1710691200" where the number after dash is Unix timestamp
get_timestamp_from_test_run_id() {
    local test_run_id="$1"
    if [ -z "$test_run_id" ]; then
        echo "0"
        return
    fi
    # Extract timestamp (everything after the last dash)
    local timestamp=$(echo "$test_run_id" | awk -F'-' '{print $NF}')
    if [[ "$timestamp" =~ ^[0-9]+$ ]]; then
        echo "$timestamp"
    else
        echo "0"
    fi
}

# Function to check if a resource is old enough to delete based on creation timestamp
# Returns 0 (true) if resource should be deleted, 1 (false) if too new
is_resource_old_enough() {
    local resource_timestamp="$1"

    # If no before-date filter specified, delete all resources
    if [ -z "$BEFORE_DATE" ] || [ "$BEFORE_TIMESTAMP" -eq 0 ]; then
        return 0
    fi

    # If resource has no timestamp, be conservative and don't delete
    if [ -z "$resource_timestamp" ] || [ "$resource_timestamp" -eq 0 ]; then
        return 1
    fi

    # Delete if resource timestamp is before the cutoff
    if [ "$resource_timestamp" -lt "$BEFORE_TIMESTAMP" ]; then
        return 0
    else
        return 1
    fi
}

# Function to convert ISO 8601 datetime to Unix timestamp
# Input: 2026-03-17T12:34:56.789Z
# Output: Unix timestamp
iso8601_to_timestamp() {
    local iso_date="$1"
    if [ -z "$iso_date" ]; then
        echo "0"
        return
    fi

    # Remove milliseconds and timezone for easier parsing
    # 2026-03-17T12:34:56.789Z -> 2026-03-17T12:34:56
    local clean_date=$(echo "$iso_date" | sed 's/\.[0-9]*Z$//' | sed 's/Z$//' | sed 's/\+.*//')

    # Convert to timestamp
    if date -j -f "%Y-%m-%dT%H:%M:%S" "$clean_date" "+%s" >/dev/null 2>&1; then
        # macOS
        date -j -f "%Y-%m-%dT%H:%M:%S" "$clean_date" "+%s"
    elif date -d "$clean_date" "+%s" >/dev/null 2>&1; then
        # Linux
        date -d "$clean_date" "+%s"
    else
        echo "0"
    fi
}

# Function to delete resources by type
delete_resources() {
    local resource_type=$1
    local delete_command=$2

    echo "Finding $resource_type..."
    if [ -n "$TEST_RUN_ID" ]; then
        resources=$(aws resourcegroupstaggingapi get-resources \
            --region "$REGION" \
            --tag-filters "Key=$TAG_KEY,Values=$TAG_VALUE" "Key=TestRunID,Values=$TEST_RUN_ID" \
            --resource-type-filters "$resource_type" \
            --query 'ResourceTagMappingList[].ResourceARN' \
            --output text)
    else
        resources=$(aws resourcegroupstaggingapi get-resources \
            --region "$REGION" \
            --tag-filters "Key=$TAG_KEY,Values=$TAG_VALUE" \
            --resource-type-filters "$resource_type" \
            --query 'ResourceTagMappingList[].ResourceARN' \
            --output text)
    fi

    if [ -z "$resources" ]; then
        echo "No $resource_type found"
        return
    fi

    for arn in $resources; do
        resource_id=$(echo "$arn" | awk -F'/' '{print $NF}')
        echo "Deleting $resource_type: $resource_id"
        eval "$delete_command $resource_id" || echo "Failed to delete $resource_id (may already be deleted)"
    done
}

# 1. Delete EKS clusters
if [ "$DRY_RUN" = false ]; then
    echo "Step 1: Deleting EKS clusters..."
else
    echo "Step 1: Finding EKS clusters..."
fi

# Get AWS account ID for ARN construction
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

# If specific cluster name provided, only process that cluster
if [ -n "$CLUSTER_NAME" ]; then
    # Check if cluster exists
    if aws eks describe-cluster --region "$REGION" --name "$CLUSTER_NAME" &>/dev/null; then
        cluster_arn="arn:aws:eks:$REGION:$ACCOUNT_ID:cluster/$CLUSTER_NAME"
        tags=$(aws eks list-tags-for-resource --region "$REGION" --resource-arn "$cluster_arn" --query 'tags' --output json 2>/dev/null || echo "{}")
        cluster_test_run_id=$(get_test_run_id "$tags")

        # Store TestRunID for later cleanup of associated resources
        TEST_RUN_ID="$cluster_test_run_id"

        if [ "$DRY_RUN" = false ]; then
            echo "Deleting EKS cluster: $CLUSTER_NAME"
            [ -n "$cluster_test_run_id" ] && echo "  TestRunID: $cluster_test_run_id"
            [ -n "$cluster_test_run_id" ] && echo "  Will also delete all resources with TestRunID: $cluster_test_run_id"
            eksctl delete cluster --name "$CLUSTER_NAME" --region "$REGION" --force --disable-nodegroup-eviction --wait || true
        else
            echo "[DRY RUN] Would delete EKS cluster: $CLUSTER_NAME"
            [ -n "$cluster_test_run_id" ] && echo "  TestRunID: $cluster_test_run_id"
            [ -n "$cluster_test_run_id" ] && echo "  Would also delete all resources with TestRunID: $cluster_test_run_id"
        fi
    else
        echo "Cluster $CLUSTER_NAME not found in region $REGION"
        exit 1
    fi
else
    # Process all clusters matching pattern
    clusters=$(aws eks list-clusters --region "$REGION" --query 'clusters' --output text)
    for cluster in $clusters; do
        if [[ $cluster == aws-chart-testing-cluster-* ]]; then
            # Get cluster details including creation time
            cluster_arn="arn:aws:eks:$REGION:$ACCOUNT_ID:cluster/$cluster"
            cluster_info=$(aws eks describe-cluster --region "$REGION" --name "$cluster" --query 'cluster' --output json 2>/dev/null || echo "{}")
            created_at=$(echo "$cluster_info" | jq -r '.createdAt // ""' 2>/dev/null || echo "")

            tags=$(aws eks list-tags-for-resource --region "$REGION" --resource-arn "$cluster_arn" --query 'tags' --output json 2>/dev/null || echo "{}")
            cluster_test_run_id=$(get_test_run_id "$tags")

            # Check if this cluster matches our TestRunID filter
            if [ -n "$TEST_RUN_ID" ] && [ "$cluster_test_run_id" != "$TEST_RUN_ID" ]; then
                # Skip this cluster - doesn't match our TestRunID
                continue
            fi

            # If --all mode and cluster has no TestRunID tag, check if it's a helm-charts-e2e cluster
            if [ -z "$TEST_RUN_ID" ] && [ -z "$cluster_test_run_id" ]; then
                # Check if cluster has ManagedBy=helm-charts-e2e tag
                managed_by=$(echo "$tags" | jq -r '.ManagedBy // ""' 2>/dev/null || echo "")
                if [ "$managed_by" != "helm-charts-e2e" ]; then
                    # Skip - not a helm-charts-e2e cluster
                    continue
                fi
            fi

            # Check if cluster is old enough to delete (if --before-date specified)
            if [ -n "$BEFORE_DATE" ]; then
                cluster_timestamp=$(iso8601_to_timestamp "$created_at")
                if ! is_resource_old_enough "$cluster_timestamp"; then
                    if [ "$DRY_RUN" = false ]; then
                        echo "Skipping EKS cluster (too new): $cluster (created: $created_at)"
                    else
                        echo "[DRY RUN] Would skip EKS cluster (too new): $cluster (created: $created_at)"
                    fi
                    continue
                fi
            fi

            if [ "$DRY_RUN" = false ]; then
                echo "Deleting EKS cluster: $cluster"
                [ -n "$created_at" ] && echo "  Created: $created_at"
                [ -n "$cluster_test_run_id" ] && echo "  TestRunID: $cluster_test_run_id"
                eksctl delete cluster --name "$cluster" --region "$REGION" --force --disable-nodegroup-eviction --wait || true
            else
                echo "[DRY RUN] Would delete EKS cluster: $cluster"
                [ -n "$created_at" ] && echo "  Created: $created_at"
                [ -n "$cluster_test_run_id" ] && echo "  TestRunID: $cluster_test_run_id"
            fi
        fi
    done
fi

# 2. Delete CloudFormation stacks (in case eksctl failed)
if [ "$DRY_RUN" = false ]; then
    echo "Step 2: Deleting CloudFormation stacks..."
else
    echo "Step 2: Finding CloudFormation stacks..."
fi

# If specific cluster name was provided, look for stacks for that cluster specifically
if [ -n "$CLUSTER_NAME" ]; then
    # Find stacks for the specific cluster
    stacks=$(aws cloudformation list-stacks \
        --region "$REGION" \
        --stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE ROLLBACK_COMPLETE \
        --query "StackSummaries[?starts_with(StackName, \`eksctl-$CLUSTER_NAME\`)].StackName" \
        --output text)
else
    # Find all matching stacks
    stacks=$(aws cloudformation list-stacks \
        --region "$REGION" \
        --stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE ROLLBACK_COMPLETE \
        --query 'StackSummaries[?starts_with(StackName, `eksctl-aws-chart-testing`)].StackName' \
        --output text)
fi

for stack in $stacks; do
    # Get stack details including creation time and tags
    stack_info=$(aws cloudformation describe-stacks --region "$REGION" --stack-name "$stack" --query 'Stacks[0]' --output json 2>/dev/null || echo "{}")
    tags=$(echo "$stack_info" | jq -r '.Tags // []' 2>/dev/null || echo "[]")
    created_time=$(echo "$stack_info" | jq -r '.CreationTime // ""' 2>/dev/null || echo "")
    stack_test_run_id=$(get_test_run_id "$tags")

    # Check if this stack matches our TestRunID filter
    if [ -n "$TEST_RUN_ID" ] && [ "$stack_test_run_id" != "$TEST_RUN_ID" ]; then
        # Skip this stack - doesn't match our TestRunID
        continue
    fi

    # If --all mode and stack has no TestRunID tag, check if it's a helm-charts-e2e stack
    if [ -z "$TEST_RUN_ID" ] && [ -z "$CLUSTER_NAME" ] && [ -z "$stack_test_run_id" ]; then
        # Check if stack has ManagedBy=helm-charts-e2e tag
        managed_by=$(echo "$tags" | jq -r '.[] | select(.Key=="ManagedBy") | .Value' 2>/dev/null || echo "")
        if [ "$managed_by" != "helm-charts-e2e" ]; then
            # Skip - not a helm-charts-e2e stack
            continue
        fi
    fi

    # Check if stack is old enough to delete (if --before-date specified)
    if [ -n "$BEFORE_DATE" ]; then
        stack_timestamp=$(iso8601_to_timestamp "$created_time")
        if ! is_resource_old_enough "$stack_timestamp"; then
            if [ "$DRY_RUN" = false ]; then
                echo "Skipping CloudFormation stack (too new): $stack (created: $created_time)"
            else
                echo "[DRY RUN] Would skip CloudFormation stack (too new): $stack (created: $created_time)"
            fi
            continue
        fi
    fi

    if [ "$DRY_RUN" = false ]; then
        echo "Processing stack: $stack"
        [ -n "$created_time" ] && echo "  Created: $created_time"
        [ -n "$stack_test_run_id" ] && echo "  TestRunID: $stack_test_run_id"
        # Disable termination protection
        aws cloudformation update-termination-protection \
            --region "$REGION" \
            --stack-name "$stack" \
            --no-enable-termination-protection 2>/dev/null || true
        # Delete stack
        aws cloudformation delete-stack \
            --region "$REGION" \
            --stack-name "$stack" 2>/dev/null || true
    else
        echo "[DRY RUN] Would delete CloudFormation stack: $stack"
        [ -n "$created_time" ] && echo "  Created: $created_time"
        [ -n "$stack_test_run_id" ] && echo "  TestRunID: $stack_test_run_id"
    fi
done

# Wait a bit for resources to be released
if [ "$DRY_RUN" = false ]; then
    echo "Waiting for cluster resources to be released..."
    sleep 30
fi

# 3. Delete Security Groups (retry needed due to dependencies)
if [ "$DRY_RUN" = false ]; then
    echo "Step 3: Deleting Security Groups..."
else
    echo "Step 3: Finding Security Groups..."
fi
for i in {1..3}; do
    if [ -n "$TEST_RUN_ID" ]; then
        sgs=$(aws ec2 describe-security-groups \
            --region "$REGION" \
            --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
            --query 'SecurityGroups[].GroupId' \
            --output text)
    else
        sgs=$(aws ec2 describe-security-groups \
            --region "$REGION" \
            --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
            --query 'SecurityGroups[].GroupId' \
            --output text)
    fi

    if [ -z "$sgs" ]; then
        echo "No security groups found"
        break
    fi

    for sg in $sgs; do
        # Get security group tags
        tags=$(aws ec2 describe-security-groups --region "$REGION" --group-ids "$sg" --query 'SecurityGroups[0].Tags' --output json 2>/dev/null || echo "[]")
        test_run_id=$(get_test_run_id "$tags")

        # Check if resource is old enough to delete (if --before-date specified)
        if [ -n "$BEFORE_DATE" ]; then
            resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
            if ! is_resource_old_enough "$resource_timestamp"; then
                continue
            fi
        fi

        if [ "$DRY_RUN" = false ]; then
            echo "Attempt $i: Deleting security group $sg"
            [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
            aws ec2 delete-security-group --region "$REGION" --group-id "$sg" 2>/dev/null || true
        else
            echo "[DRY RUN] Would delete security group: $sg"
            [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        fi
    done

    if [ "$DRY_RUN" = false ] && [ $i -lt 3 ]; then
        echo "Waiting 10s before retry..."
        sleep 10
    fi

    # Only retry once in dry-run mode
    if [ "$DRY_RUN" = true ]; then
        break
    fi
done

# 4. Delete Subnets
if [ "$DRY_RUN" = false ]; then
    echo "Step 4: Deleting Subnets..."
else
    echo "Step 4: Finding Subnets..."
fi
if [ -n "$TEST_RUN_ID" ]; then
    subnets=$(aws ec2 describe-subnets \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
        --query 'Subnets[].SubnetId' \
        --output text)
else
    subnets=$(aws ec2 describe-subnets \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
        --query 'Subnets[].SubnetId' \
        --output text)
fi

for subnet in $subnets; do
    # Get subnet tags
    tags=$(aws ec2 describe-subnets --region "$REGION" --subnet-ids "$subnet" --query 'Subnets[0].Tags' --output json 2>/dev/null || echo "[]")
    test_run_id=$(get_test_run_id "$tags")

    # Check if resource is old enough to delete (if --before-date specified)
    if [ -n "$BEFORE_DATE" ]; then
        resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
        if ! is_resource_old_enough "$resource_timestamp"; then
            if [ "$DRY_RUN" = false ]; then
                echo "Skipping subnet (too new): $subnet"
            else
                echo "[DRY RUN] Would skip subnet (too new): $subnet"
            fi
            [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
            continue
        fi
    fi

    if [ "$DRY_RUN" = false ]; then
        echo "Deleting subnet: $subnet"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        aws ec2 delete-subnet --region "$REGION" --subnet-id "$subnet" 2>/dev/null || true
    else
        echo "[DRY RUN] Would delete subnet: $subnet"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
    fi
done

# 5. Delete Route Tables
if [ "$DRY_RUN" = false ]; then
    echo "Step 5: Deleting Route Tables..."
else
    echo "Step 5: Finding Route Tables..."
fi
if [ -n "$TEST_RUN_ID" ]; then
    rtbs=$(aws ec2 describe-route-tables \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
        --query 'RouteTables[].RouteTableId' \
        --output text)
else
    rtbs=$(aws ec2 describe-route-tables \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
        --query 'RouteTables[].RouteTableId' \
        --output text)
fi

for rtb in $rtbs; do
    # Get route table tags
    tags=$(aws ec2 describe-route-tables --region "$REGION" --route-table-ids "$rtb" --query 'RouteTables[0].Tags' --output json 2>/dev/null || echo "[]")
    test_run_id=$(get_test_run_id "$tags")

    # Check if resource is old enough to delete (if --before-date specified)
    if [ -n "$BEFORE_DATE" ]; then
        resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
        if ! is_resource_old_enough "$resource_timestamp"; then
            continue
        fi
    fi

    if [ "$DRY_RUN" = false ]; then
        echo "Deleting route table: $rtb"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        # First disassociate from subnets
        associations=$(aws ec2 describe-route-tables \
            --region "$REGION" \
            --route-table-ids "$rtb" \
            --query 'RouteTables[].Associations[?SubnetId!=`null`].RouteTableAssociationId' \
            --output text)

        for assoc in $associations; do
            aws ec2 disassociate-route-table --region "$REGION" --association-id "$assoc" 2>/dev/null || true
        done

        aws ec2 delete-route-table --region "$REGION" --route-table-id "$rtb" 2>/dev/null || true
    else
        echo "[DRY RUN] Would delete route table: $rtb"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
    fi
done

# 6. Delete Internet Gateways
if [ "$DRY_RUN" = false ]; then
    echo "Step 6: Deleting Internet Gateways..."
else
    echo "Step 6: Finding Internet Gateways..."
fi
if [ -n "$TEST_RUN_ID" ]; then
    igws=$(aws ec2 describe-internet-gateways \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
        --query 'InternetGateways[].InternetGatewayId' \
        --output text)
else
    igws=$(aws ec2 describe-internet-gateways \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
        --query 'InternetGateways[].InternetGatewayId' \
        --output text)
fi

for igw in $igws; do
    # Get internet gateway tags
    tags=$(aws ec2 describe-internet-gateways --region "$REGION" --internet-gateway-ids "$igw" --query 'InternetGateways[0].Tags' --output json 2>/dev/null || echo "[]")
    test_run_id=$(get_test_run_id "$tags")

    # Check if resource is old enough to delete (if --before-date specified)
    if [ -n "$BEFORE_DATE" ]; then
        resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
        if ! is_resource_old_enough "$resource_timestamp"; then
            continue
        fi
    fi

    if [ "$DRY_RUN" = false ]; then
        echo "Deleting internet gateway: $igw"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        # First detach from VPCs
        vpcs=$(aws ec2 describe-internet-gateways \
            --region "$REGION" \
            --internet-gateway-ids "$igw" \
            --query 'InternetGateways[].Attachments[].VpcId' \
            --output text)

        for vpc in $vpcs; do
            aws ec2 detach-internet-gateway --region "$REGION" --internet-gateway-id "$igw" --vpc-id "$vpc" 2>/dev/null || true
        done

        aws ec2 delete-internet-gateway --region "$REGION" --internet-gateway-id "$igw" 2>/dev/null || true
    else
        echo "[DRY RUN] Would delete internet gateway: $igw"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
    fi
done

# 6a. Delete Load Balancers (created by Kubernetes services)
if [ "$DRY_RUN" = false ]; then
    echo "Step 6a: Deleting Load Balancers..."
else
    echo "Step 6a: Finding Load Balancers..."
fi

# Get VPCs to find their load balancers
if [ -n "$TEST_RUN_ID" ]; then
    vpc_ids=$(aws ec2 describe-vpcs \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
        --query 'Vpcs[].VpcId' \
        --output text)
else
    vpc_ids=$(aws ec2 describe-vpcs \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
        --query 'Vpcs[].VpcId' \
        --output text)
fi

for vpc_id in $vpc_ids; do
    # Find ELBv2 load balancers (ALB/NLB) in this VPC
    lbs=$(aws elbv2 describe-load-balancers \
        --region "$REGION" \
        --query "LoadBalancers[?VpcId=='$vpc_id'].LoadBalancerArn" \
        --output text 2>/dev/null || true)

    for lb_arn in $lbs; do
        lb_name=$(echo "$lb_arn" | awk -F'/' '{print $3"/"$4}')
        if [ "$DRY_RUN" = false ]; then
            echo "Deleting ELBv2 load balancer: $lb_name (VPC: $vpc_id)"
            aws elbv2 delete-load-balancer --region "$REGION" --load-balancer-arn "$lb_arn" 2>/dev/null || true
        else
            echo "[DRY RUN] Would delete ELBv2 load balancer: $lb_name (VPC: $vpc_id)"
        fi
    done

    # Find classic load balancers in this VPC
    classic_lbs=$(aws elb describe-load-balancers \
        --region "$REGION" \
        --query "LoadBalancerDescriptions[?VPCId=='$vpc_id'].LoadBalancerName" \
        --output text 2>/dev/null || true)

    for lb_name in $classic_lbs; do
        if [ "$DRY_RUN" = false ]; then
            echo "Deleting classic load balancer: $lb_name (VPC: $vpc_id)"
            aws elb delete-load-balancer --region "$REGION" --load-balancer-name "$lb_name" 2>/dev/null || true
        else
            echo "[DRY RUN] Would delete classic load balancer: $lb_name (VPC: $vpc_id)"
        fi
    done
done

# Wait for load balancers to be deleted
if [ "$DRY_RUN" = false ] && [ -n "$vpc_ids" ]; then
    echo "Waiting for load balancers to be deleted..."
    sleep 20
fi

# 6b. Delete NAT Gateways
if [ "$DRY_RUN" = false ]; then
    echo "Step 6b: Deleting NAT Gateways..."
else
    echo "Step 6b: Finding NAT Gateways..."
fi

for vpc_id in $vpc_ids; do
    nat_gws=$(aws ec2 describe-nat-gateways \
        --region "$REGION" \
        --filter "Name=vpc-id,Values=$vpc_id" "Name=state,Values=available,pending" \
        --query 'NatGateways[].NatGatewayId' \
        --output text 2>/dev/null || true)

    for nat_gw in $nat_gws; do
        if [ "$DRY_RUN" = false ]; then
            echo "Deleting NAT Gateway: $nat_gw (VPC: $vpc_id)"
            aws ec2 delete-nat-gateway --region "$REGION" --nat-gateway-id "$nat_gw" 2>/dev/null || true
        else
            echo "[DRY RUN] Would delete NAT Gateway: $nat_gw (VPC: $vpc_id)"
        fi
    done
done

# Wait for NAT gateways to be deleted
if [ "$DRY_RUN" = false ] && [ -n "$vpc_ids" ]; then
    echo "Waiting for NAT gateways to be deleted..."
    sleep 30
fi

# 6c. Delete Network Interfaces (ENIs)
if [ "$DRY_RUN" = false ]; then
    echo "Step 6c: Deleting Network Interfaces..."
else
    echo "Step 6c: Finding Network Interfaces..."
fi

for vpc_id in $vpc_ids; do
    enis=$(aws ec2 describe-network-interfaces \
        --region "$REGION" \
        --filters "Name=vpc-id,Values=$vpc_id" \
        --query 'NetworkInterfaces[?Status==`available`].NetworkInterfaceId' \
        --output text 2>/dev/null || true)

    for eni in $enis; do
        if [ "$DRY_RUN" = false ]; then
            echo "Deleting network interface: $eni (VPC: $vpc_id)"
            aws ec2 delete-network-interface --region "$REGION" --network-interface-id "$eni" 2>/dev/null || true
        else
            echo "[DRY RUN] Would delete network interface: $eni (VPC: $vpc_id)"
        fi
    done
done

# 6d. Release Elastic IPs
if [ "$DRY_RUN" = false ]; then
    echo "Step 6d: Releasing Elastic IPs..."
else
    echo "Step 6d: Finding Elastic IPs..."
fi

if [ -n "$TEST_RUN_ID" ]; then
    eips=$(aws ec2 describe-addresses \
        --region "$REGION" \
        --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
        --query 'Addresses[].AllocationId' \
        --output text 2>/dev/null || true)
else
    # Find EIPs associated with our NAT gateways (they may not have tags)
    for vpc_id in $vpc_ids; do
        eips=$(aws ec2 describe-addresses \
            --region "$REGION" \
            --query 'Addresses[?Domain==`vpc`].AllocationId' \
            --output text 2>/dev/null || true)
    done
fi

for eip in $eips; do
    # Get EIP tags if available
    tags=$(aws ec2 describe-addresses --region "$REGION" --allocation-ids "$eip" --query 'Addresses[0].Tags' --output json 2>/dev/null || echo "[]")
    test_run_id=$(get_test_run_id "$tags")

    # Check if resource is old enough to delete (if --before-date specified)
    if [ -n "$BEFORE_DATE" ] && [ -n "$test_run_id" ]; then
        resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
        if ! is_resource_old_enough "$resource_timestamp"; then
            continue
        fi
    fi

    if [ "$DRY_RUN" = false ]; then
        echo "Releasing Elastic IP: $eip"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        aws ec2 release-address --region "$REGION" --allocation-id "$eip" 2>/dev/null || true
    else
        echo "[DRY RUN] Would release Elastic IP: $eip"
        [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
    fi
done

# 6e. Delete VPC Endpoints
if [ "$DRY_RUN" = false ]; then
    echo "Step 6e: Deleting VPC Endpoints..."
else
    echo "Step 6e: Finding VPC Endpoints..."
fi

for vpc_id in $vpc_ids; do
    vpc_endpoints=$(aws ec2 describe-vpc-endpoints \
        --region "$REGION" \
        --filters "Name=vpc-id,Values=$vpc_id" \
        --query 'VpcEndpoints[].VpcEndpointId' \
        --output text 2>/dev/null || true)

    for endpoint in $vpc_endpoints; do
        if [ "$DRY_RUN" = false ]; then
            echo "Deleting VPC endpoint: $endpoint (VPC: $vpc_id)"
            aws ec2 delete-vpc-endpoints --region "$REGION" --vpc-endpoint-ids "$endpoint" 2>&1 | grep -v "does not exist" || true
        else
            echo "[DRY RUN] Would delete VPC endpoint: $endpoint (VPC: $vpc_id)"
        fi
    done
done

# 6f. Revoke Security Group Rules (required before VPC deletion)
if [ "$DRY_RUN" = false ]; then
    echo "Step 6f: Revoking Security Group Rules..."
else
    echo "Step 6f: Finding Security Group Rules..."
fi

for vpc_id in $vpc_ids; do
    sgs=$(aws ec2 describe-security-groups \
        --region "$REGION" \
        --filters "Name=vpc-id,Values=$vpc_id" \
        --query 'SecurityGroups[].GroupId' \
        --output text 2>/dev/null || true)

    for sg in $sgs; do
        if [ "$DRY_RUN" = false ]; then
            echo "Revoking rules for security group: $sg (VPC: $vpc_id)"

            # Revoke ingress rules
            ingress_rules=$(aws ec2 describe-security-groups \
                --region "$REGION" \
                --group-ids "$sg" \
                --query 'SecurityGroups[0].IpPermissions' \
                --output json 2>/dev/null)

            if [ "$ingress_rules" != "[]" ] && [ "$ingress_rules" != "null" ] && [ -n "$ingress_rules" ]; then
                echo "  Revoking ingress rules..."
                aws ec2 revoke-security-group-ingress \
                    --region "$REGION" \
                    --group-id "$sg" \
                    --ip-permissions "$ingress_rules" 2>&1 | grep -v "does not exist" || true
            fi

            # Revoke egress rules (except for VPC default SG, keep one egress rule)
            sg_name=$(aws ec2 describe-security-groups \
                --region "$REGION" \
                --group-ids "$sg" \
                --query 'SecurityGroups[0].GroupName' \
                --output text 2>/dev/null)

            if [ "$sg_name" != "default" ]; then
                egress_rules=$(aws ec2 describe-security-groups \
                    --region "$REGION" \
                    --group-ids "$sg" \
                    --query 'SecurityGroups[0].IpPermissionsEgress' \
                    --output json 2>/dev/null)

                if [ "$egress_rules" != "[]" ] && [ "$egress_rules" != "null" ] && [ -n "$egress_rules" ]; then
                    echo "  Revoking egress rules..."
                    aws ec2 revoke-security-group-egress \
                        --region "$REGION" \
                        --group-id "$sg" \
                        --ip-permissions "$egress_rules" 2>&1 | grep -v "does not exist" || true
                fi
            fi
        else
            echo "[DRY RUN] Would revoke rules for security group: $sg (VPC: $vpc_id)"
        fi
    done
done

# Wait a bit more for all dependencies to clear
if [ "$DRY_RUN" = false ] && [ -n "$vpc_ids" ]; then
    echo "Waiting for VPC dependencies to be fully cleared..."
    sleep 15
fi

# 7. Delete VPCs (retry needed)
if [ "$DRY_RUN" = false ]; then
    echo "Step 7: Deleting VPCs..."
else
    echo "Step 7: Finding VPCs..."
fi
for i in {1..3}; do
    if [ -n "$TEST_RUN_ID" ]; then
        vpcs=$(aws ec2 describe-vpcs \
            --region "$REGION" \
            --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" "Name=tag:TestRunID,Values=$TEST_RUN_ID" \
            --query 'Vpcs[].VpcId' \
            --output text)
    else
        vpcs=$(aws ec2 describe-vpcs \
            --region "$REGION" \
            --filters "Name=tag:$TAG_KEY,Values=$TAG_VALUE" \
            --query 'Vpcs[].VpcId' \
            --output text)
    fi

    if [ -z "$vpcs" ]; then
        echo "No VPCs found"
        break
    fi

    for vpc in $vpcs; do
        # Get VPC tags
        tags=$(aws ec2 describe-vpcs --region "$REGION" --vpc-ids "$vpc" --query 'Vpcs[0].Tags' --output json 2>/dev/null || echo "[]")
        test_run_id=$(get_test_run_id "$tags")

        # Check if resource is old enough to delete (if --before-date specified)
        if [ -n "$BEFORE_DATE" ]; then
            resource_timestamp=$(get_timestamp_from_test_run_id "$test_run_id")
            if ! is_resource_old_enough "$resource_timestamp"; then
                if [ "$DRY_RUN" = false ]; then
                    echo "Skipping VPC (too new): $vpc"
                else
                    echo "[DRY RUN] Would skip VPC (too new): $vpc"
                fi
                [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
                continue
            fi
        fi

        if [ "$DRY_RUN" = false ]; then
            echo "Attempt $i: Deleting VPC $vpc"
            [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
            if ! aws ec2 delete-vpc --region "$REGION" --vpc-id "$vpc" 2>&1; then
                echo "  Warning: Failed to delete VPC $vpc (may have dependencies still clearing)"
            fi
        else
            echo "[DRY RUN] Would delete VPC: $vpc"
            [ -n "$test_run_id" ] && echo "  TestRunID: $test_run_id"
        fi
    done

    if [ "$DRY_RUN" = false ] && [ $i -lt 3 ]; then
        echo "Waiting 15s before retry..."
        sleep 15
    fi

    # Only retry once in dry-run mode
    if [ "$DRY_RUN" = true ]; then
        break
    fi
done

if [ "$DRY_RUN" = false ]; then
    echo "Cleanup complete!"
    if [ -n "$BEFORE_DATE" ]; then
        echo "Deleted resources created before $BEFORE_DATE"
    fi
    echo "Note: Some resources may still be deleting in the background"
    echo "Check CloudFormation console to verify all stacks are deleted"
else
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "🔍 DRY RUN COMPLETE - No resources were deleted"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    if [ -n "$BEFORE_DATE" ]; then
        echo "Resources shown were created before $BEFORE_DATE"
    fi
    echo "To actually delete these resources, run the same command without --dry-run"
fi
