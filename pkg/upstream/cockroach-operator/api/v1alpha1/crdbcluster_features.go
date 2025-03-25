package v1alpha1

// ClusterFeature is an enumeration of available features for the operator.
// These can be used to enable/disable certain features such as which objects
// should be reconciled by the operator (as opposed to intrusion).
type ClusterFeature string

const (
	// ClusterSqlProxyDisableConnectionRebalancing disables the connection
	// rebalancing feature within sqlproxy. This will only apply to host
	// clusters with sqlproxy v22.1+ images.
	ClusterSqlProxyDisableConnectionRebalancing ClusterFeature = "disable-sqlproxy-connection-rebalancing"

	// ClusterExportCustomerLogs enables log export to a second
	// Fluentbit port that's used for sending logs to customer cloud
	// sinks.
	ClusterExportCustomerLogs ClusterFeature = "export-customer-logs"

	// ClusterSqlProxyProxyProtocol ensures that the sqlproxy instances require
	// the proxy protocol for their SQL listeners. This should only be turned on
	// for *new* v23.1+ AWS host clusters (that have the AWS load balancer
	// controller installed).
	ClusterSqlProxyProxyProtocol ClusterFeature = "sqlproxy-proxy-protocol"

	// ClusterSqlProxyProxyProtocolListenAddrEnabled controls whether the proxy protocol
	// listen addr flag is passed into SQLProxy.
	ClusterSqlProxyProxyProtocolListenAddrEnabled ClusterFeature = "sqlproxy-proxy-protocol-listen-addr-enabled"

	// ClusterExplicitNodePlacement changes how CrdbNodes are mapped to k8s
	// nodes. Instead of allowing the scheduler to pick the k8s node, the
	// cluster reconciler picks the k8s node. This allows the CrdbCluster to
	// know the specific properties of the k8s node and adjust parameters like
	// cpu request and limit.
	ClusterExplicitNodePlacement ClusterFeature = "explicit-node-placement"

	// ClusterDisableGCPLiveMigrations tells the operator to disable
	// live migrations on nodes in clusters running on GCP. Instead,
	// nodes will be terminated and rebooted if they need
	// maintenance. This feature is safe to enable for clouds other
	// than GCP - the operator will treat it as a no-op.
	ClusterDisableGCPLiveMigrations ClusterFeature = "disable-gcp-live-migrations"
)
