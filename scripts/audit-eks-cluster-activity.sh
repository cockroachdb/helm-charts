#!/bin/bash
# audit-eks-cluster-activity.sh
# Audit all EKS clusters across regions for inactivity
#
# This script identifies:
#   1. Clusters with no activity (no resources) for last 1 month
#   2. Clusters with idle pods (pods not created/upgraded for last 1 month)
#
# Multi-Region Support:
#   By default, scans all enabled AWS regions
#   Use AWS_REGION to scan only a single region
#   Use AWS_REGIONS to specify custom regions (comma-separated)
#
# Usage:
#   ./audit-eks-cluster-activity.sh [--days N] [--output-format json|table]
#
# Examples:
#   ./audit-eks-cluster-activity.sh                          # Scan all regions, 30 days threshold
#   ./audit-eks-cluster-activity.sh --days 60                # Use 60 days threshold
#   AWS_REGION=us-east-1 ./audit-eks-cluster-activity.sh     # Scan only us-east-1
#   AWS_REGIONS=us-east-1,us-west-2 ./audit-eks-cluster-activity.sh  # Scan specific regions
#   ./audit-eks-cluster-activity.sh --output-format json     # Output as JSON

set -e

# Default configuration
DAYS_THRESHOLD=30
OUTPUT_FORMAT="table"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --days)
            if [ -z "$2" ]; then
                echo "Error: --days requires a number"
                exit 1
            fi
            DAYS_THRESHOLD="$2"
            shift 2
            ;;
        --output-format)
            if [ -z "$2" ]; then
                echo "Error: --output-format requires json or table"
                exit 1
            fi
            OUTPUT_FORMAT="$2"
            shift 2
            ;;
        --help|-h)
            echo "Usage: $0 [--days N] [--output-format json|table]"
            echo ""
            echo "Options:"
            echo "  --days N              Number of days to consider inactive (default: 30)"
            echo "  --output-format fmt   Output format: table (default) or json"
            echo ""
            echo "Environment Variables:"
            echo "  AWS_REGION           Scan only this region"
            echo "  AWS_REGIONS          Comma-separated list of regions to scan"
            echo ""
            echo "Examples:"
            echo "  $0"
            echo "  $0 --days 60"
            echo "  AWS_REGION=us-east-1 $0"
            echo "  AWS_REGIONS=us-east-1,us-west-2 $0"
            echo "  $0 --output-format json"
            exit 0
            ;;
        *)
            echo "Error: Unknown argument '$1'"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Validate output format
if [ "$OUTPUT_FORMAT" != "table" ] && [ "$OUTPUT_FORMAT" != "json" ]; then
    echo "Error: --output-format must be 'table' or 'json'"
    exit 1
fi

# Support both single region (AWS_REGION) and multi-region (AWS_REGIONS)
# Default to all enabled regions
if [ -n "$AWS_REGIONS" ]; then
    # Multi-region mode: AWS_REGIONS="us-east-1,us-east-2"
    IFS=',' read -ra REGIONS <<< "$AWS_REGIONS"
elif [ -n "$AWS_REGION" ]; then
    # Single region mode: AWS_REGION="us-east-1"
    REGIONS=("$AWS_REGION")
else
    # Default: all enabled regions
    echo "Discovering enabled AWS regions..." >&2
    REGIONS=($(aws ec2 describe-regions --query 'Regions[].RegionName' --output text --all-regions | tr '\t' ' '))
    if [ ${#REGIONS[@]} -eq 0 ]; then
        echo "Error: Could not discover AWS regions" >&2
        exit 1
    fi
    echo "Found ${#REGIONS[@]} regions to scan" >&2
fi

# Calculate cutoff timestamp (N days ago)
if date -v-${DAYS_THRESHOLD}d "+%s" >/dev/null 2>&1; then
    # macOS
    CUTOFF_TIMESTAMP=$(date -v-${DAYS_THRESHOLD}d "+%s")
    CUTOFF_DATE=$(date -v-${DAYS_THRESHOLD}d "+%Y-%m-%d")
elif date -d "${DAYS_THRESHOLD} days ago" "+%s" >/dev/null 2>&1; then
    # Linux
    CUTOFF_TIMESTAMP=$(date -d "${DAYS_THRESHOLD} days ago" "+%s")
    CUTOFF_DATE=$(date -d "${DAYS_THRESHOLD} days ago" "+%Y-%m-%d")
else
    echo "Error: Unsupported date command" >&2
    exit 1
fi

# Function to convert ISO 8601 datetime to Unix timestamp
iso8601_to_timestamp() {
    local iso_date="$1"
    if [ -z "$iso_date" ]; then
        echo "0"
        return
    fi

    # Remove milliseconds and timezone for easier parsing
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

# Function to convert Kubernetes timestamp to Unix timestamp
# K8s format: 2024-03-20T10:30:45Z
k8s_to_timestamp() {
    iso8601_to_timestamp "$1"
}

# Results arrays
declare -a CATEGORY1_CLUSTERS  # No activity at all
declare -a CATEGORY2_CLUSTERS  # Idle pods
declare -a ACTIVE_CLUSTERS     # Active clusters

# JSON output array
JSON_OUTPUT="["

if [ "$OUTPUT_FORMAT" = "table" ]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "EKS Cluster Activity Audit"
    echo "Threshold: ${DAYS_THRESHOLD} days (cutoff date: ${CUTOFF_DATE})"
    echo "Regions: ${REGIONS[*]}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
fi

# Loop through each region
for REGION in "${REGIONS[@]}"; do
    if [ "$OUTPUT_FORMAT" = "table" ]; then
        echo "Scanning region: $REGION"
        echo "─────────────────────────────────────────────────────────────"
    fi

    # List all EKS clusters in the region
    clusters=$(aws eks list-clusters --region "$REGION" --query 'clusters' --output text 2>/dev/null || echo "")

    if [ -z "$clusters" ]; then
        if [ "$OUTPUT_FORMAT" = "table" ]; then
            echo "  No EKS clusters found"
            echo ""
        fi
        continue
    fi

    for cluster_name in $clusters; do
        if [ "$OUTPUT_FORMAT" = "table" ]; then
            echo ""
            echo "  Cluster: $cluster_name"
        fi

        # Get cluster details
        cluster_info=$(aws eks describe-cluster --region "$REGION" --name "$cluster_name" --query 'cluster' --output json 2>/dev/null || echo "{}")
        created_at=$(echo "$cluster_info" | jq -r '.createdAt // ""' 2>/dev/null || echo "")
        cluster_status=$(echo "$cluster_info" | jq -r '.status // "UNKNOWN"' 2>/dev/null || echo "UNKNOWN")
        cluster_version=$(echo "$cluster_info" | jq -r '.version // "unknown"' 2>/dev/null || echo "unknown")

        created_timestamp=$(iso8601_to_timestamp "$created_at")

        if [ "$OUTPUT_FORMAT" = "table" ]; then
            echo "    Created: $created_at"
            echo "    Status: $cluster_status"
            echo "    Version: $cluster_version"
        fi

        # Check if cluster is accessible
        if [ "$cluster_status" != "ACTIVE" ]; then
            if [ "$OUTPUT_FORMAT" = "table" ]; then
                echo "    ⚠️  Cluster is not ACTIVE, skipping pod check"
            fi

            CATEGORY1_CLUSTERS+=("$REGION:$cluster_name:$cluster_status:$created_at:N/A:Cluster not active")

            if [ "$OUTPUT_FORMAT" = "json" ]; then
                if [ "$JSON_OUTPUT" != "[" ]; then
                    JSON_OUTPUT="${JSON_OUTPUT},"
                fi
                JSON_OUTPUT="${JSON_OUTPUT}{\"region\":\"$REGION\",\"cluster\":\"$cluster_name\",\"status\":\"$cluster_status\",\"created\":\"$created_at\",\"version\":\"$cluster_version\",\"category\":\"no_activity\",\"reason\":\"Cluster not active\",\"pod_count\":0,\"newest_pod_age\":null}"
            fi
            continue
        fi

        # Update kubeconfig to access the cluster
        if ! aws eks update-kubeconfig --region "$REGION" --name "$cluster_name" --kubeconfig /tmp/kubeconfig-audit-$$ >/dev/null 2>&1; then
            if [ "$OUTPUT_FORMAT" = "table" ]; then
                echo "    ⚠️  Failed to configure kubectl access"
            fi

            CATEGORY1_CLUSTERS+=("$REGION:$cluster_name:$cluster_status:$created_at:N/A:Failed to configure kubectl")

            if [ "$OUTPUT_FORMAT" = "json" ]; then
                if [ "$JSON_OUTPUT" != "[" ]; then
                    JSON_OUTPUT="${JSON_OUTPUT},"
                fi
                JSON_OUTPUT="${JSON_OUTPUT}{\"region\":\"$REGION\",\"cluster\":\"$cluster_name\",\"status\":\"$cluster_status\",\"created\":\"$created_at\",\"version\":\"$cluster_version\",\"category\":\"no_activity\",\"reason\":\"Failed to configure kubectl\",\"pod_count\":0,\"newest_pod_age\":null}"
            fi
            continue
        fi

        # Get all pods across all namespaces
        pods_json=$(kubectl --kubeconfig /tmp/kubeconfig-audit-$$ get pods --all-namespaces -o json 2>/dev/null || echo '{"items":[]}')
        pod_count=$(echo "$pods_json" | jq '.items | length' 2>/dev/null || echo "0")

        if [ "$OUTPUT_FORMAT" = "table" ]; then
            echo "    Total pods: $pod_count"
        fi

        # Category 1: No pods at all
        if [ "$pod_count" -eq 0 ]; then
            if [ "$OUTPUT_FORMAT" = "table" ]; then
                echo "    📊 CATEGORY 1: No activity (no pods running)"
            fi

            CATEGORY1_CLUSTERS+=("$REGION:$cluster_name:$cluster_status:$created_at:0:No pods")

            if [ "$OUTPUT_FORMAT" = "json" ]; then
                if [ "$JSON_OUTPUT" != "[" ]; then
                    JSON_OUTPUT="${JSON_OUTPUT},"
                fi
                JSON_OUTPUT="${JSON_OUTPUT}{\"region\":\"$REGION\",\"cluster\":\"$cluster_name\",\"status\":\"$cluster_status\",\"created\":\"$created_at\",\"version\":\"$cluster_version\",\"category\":\"no_activity\",\"reason\":\"No pods\",\"pod_count\":0,\"newest_pod_age\":null}"
            fi
            continue
        fi

        # Check pod creation times to determine activity
        newest_pod_timestamp=0
        newest_pod_name=""
        newest_pod_namespace=""
        newest_pod_date=""

        # Iterate through all pods
        for i in $(seq 0 $((pod_count - 1))); do
            pod_name=$(echo "$pods_json" | jq -r ".items[$i].metadata.name" 2>/dev/null)
            pod_namespace=$(echo "$pods_json" | jq -r ".items[$i].metadata.namespace" 2>/dev/null)
            creation_time=$(echo "$pods_json" | jq -r ".items[$i].metadata.creationTimestamp" 2>/dev/null)

            pod_timestamp=$(k8s_to_timestamp "$creation_time")

            if [ "$pod_timestamp" -gt "$newest_pod_timestamp" ]; then
                newest_pod_timestamp=$pod_timestamp
                newest_pod_name=$pod_name
                newest_pod_namespace=$pod_namespace
                newest_pod_date=$creation_time
            fi
        done

        # Calculate age in days
        current_timestamp=$(date +%s)
        newest_pod_age_seconds=$((current_timestamp - newest_pod_timestamp))
        newest_pod_age_days=$((newest_pod_age_seconds / 86400))

        if [ "$OUTPUT_FORMAT" = "table" ]; then
            echo "    Newest pod: $newest_pod_namespace/$newest_pod_name"
            echo "    Newest pod created: $newest_pod_date (${newest_pod_age_days} days ago)"
        fi

        # Categorize based on newest pod age
        if [ "$newest_pod_timestamp" -lt "$CUTOFF_TIMESTAMP" ]; then
            # Category 2: Pods exist but haven't been created/updated recently
            if [ "$OUTPUT_FORMAT" = "table" ]; then
                echo "    📊 CATEGORY 2: Idle pods (no pod activity for ${newest_pod_age_days} days)"
            fi

            CATEGORY2_CLUSTERS+=("$REGION:$cluster_name:$cluster_status:$created_at:$pod_count:Idle (${newest_pod_age_days}d):$newest_pod_namespace/$newest_pod_name")

            if [ "$OUTPUT_FORMAT" = "json" ]; then
                if [ "$JSON_OUTPUT" != "[" ]; then
                    JSON_OUTPUT="${JSON_OUTPUT},"
                fi
                JSON_OUTPUT="${JSON_OUTPUT}{\"region\":\"$REGION\",\"cluster\":\"$cluster_name\",\"status\":\"$cluster_status\",\"created\":\"$created_at\",\"version\":\"$cluster_version\",\"category\":\"idle_pods\",\"reason\":\"No pod created/upgraded in ${newest_pod_age_days} days\",\"pod_count\":$pod_count,\"newest_pod_age\":${newest_pod_age_days},\"newest_pod\":\"$newest_pod_namespace/$newest_pod_name\",\"newest_pod_created\":\"$newest_pod_date\"}"
            fi
        else
            # Active cluster
            if [ "$OUTPUT_FORMAT" = "table" ]; then
                echo "    ✅ ACTIVE: Recent pod activity (${newest_pod_age_days} days ago)"
            fi

            ACTIVE_CLUSTERS+=("$REGION:$cluster_name:$cluster_status:$created_at:$pod_count:Active (${newest_pod_age_days}d):$newest_pod_namespace/$newest_pod_name")

            if [ "$OUTPUT_FORMAT" = "json" ]; then
                if [ "$JSON_OUTPUT" != "[" ]; then
                    JSON_OUTPUT="${JSON_OUTPUT},"
                fi
                JSON_OUTPUT="${JSON_OUTPUT}{\"region\":\"$REGION\",\"cluster\":\"$cluster_name\",\"status\":\"$cluster_status\",\"created\":\"$created_at\",\"version\":\"$cluster_version\",\"category\":\"active\",\"reason\":\"Pod activity within threshold\",\"pod_count\":$pod_count,\"newest_pod_age\":${newest_pod_age_days},\"newest_pod\":\"$newest_pod_namespace/$newest_pod_name\",\"newest_pod_created\":\"$newest_pod_date\"}"
            fi
        fi
    done

    if [ "$OUTPUT_FORMAT" = "table" ]; then
        echo ""
    fi
done

# Cleanup temp kubeconfig
rm -f /tmp/kubeconfig-audit-$$

# Output summary
if [ "$OUTPUT_FORMAT" = "json" ]; then
    JSON_OUTPUT="${JSON_OUTPUT}]"
    echo "$JSON_OUTPUT"
else
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "SUMMARY"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "CATEGORY 1: Clusters with no activity (${#CATEGORY1_CLUSTERS[@]} clusters)"
    echo "────────────────────────────────────────────────────────────────"
    if [ ${#CATEGORY1_CLUSTERS[@]} -eq 0 ]; then
        echo "  (none)"
    else
        printf "%-20s %-50s %-15s %-30s %-10s %s\n" "REGION" "CLUSTER" "STATUS" "CREATED" "PODS" "REASON"
        echo "  ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────"
        for entry in "${CATEGORY1_CLUSTERS[@]}"; do
            IFS=':' read -r region cluster status created pods reason <<< "$entry"
            printf "  %-20s %-50s %-15s %-30s %-10s %s\n" "$region" "$cluster" "$status" "$created" "$pods" "$reason"
        done
    fi

    echo ""
    echo "CATEGORY 2: Clusters with idle pods (${#CATEGORY2_CLUSTERS[@]} clusters)"
    echo "────────────────────────────────────────────────────────────────"
    if [ ${#CATEGORY2_CLUSTERS[@]} -eq 0 ]; then
        echo "  (none)"
    else
        printf "%-20s %-50s %-15s %-10s %-20s %s\n" "REGION" "CLUSTER" "STATUS" "PODS" "IDLE STATUS" "NEWEST POD"
        echo "  ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────"
        for entry in "${CATEGORY2_CLUSTERS[@]}"; do
            IFS=':' read -r region cluster status created pods idle_status newest_pod <<< "$entry"
            printf "  %-20s %-50s %-15s %-10s %-20s %s\n" "$region" "$cluster" "$status" "$pods" "$idle_status" "$newest_pod"
        done
    fi

    echo ""
    echo "Active clusters (${#ACTIVE_CLUSTERS[@]} clusters)"
    echo "────────────────────────────────────────────────────────────────"
    if [ ${#ACTIVE_CLUSTERS[@]} -eq 0 ]; then
        echo "  (none)"
    else
        printf "%-20s %-50s %-15s %-10s %-20s %s\n" "REGION" "CLUSTER" "STATUS" "PODS" "ACTIVITY" "NEWEST POD"
        echo "  ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────"
        for entry in "${ACTIVE_CLUSTERS[@]}"; do
            IFS=':' read -r region cluster status created pods activity newest_pod <<< "$entry"
            printf "  %-20s %-50s %-15s %-10s %-20s %s\n" "$region" "$cluster" "$status" "$pods" "$activity" "$newest_pod"
        done
    fi

    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "Total clusters scanned: $((${#CATEGORY1_CLUSTERS[@]} + ${#CATEGORY2_CLUSTERS[@]} + ${#ACTIVE_CLUSTERS[@]}))"
    echo "  - No activity: ${#CATEGORY1_CLUSTERS[@]}"
    echo "  - Idle pods: ${#CATEGORY2_CLUSTERS[@]}"
    echo "  - Active: ${#ACTIVE_CLUSTERS[@]}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
fi
