module github.com/cockroachdb/helm-charts

go 1.15

require (
	github.com/cenkalti/backoff v2.2.1+incompatible
	github.com/go-logr/logr v0.4.0 // indirect
	github.com/google/martian v2.1.0+incompatible
	github.com/gruntwork-io/terratest v0.36.0
	github.com/mitchellh/hashstructure/v2 v2.0.2
	github.com/pkg/errors v0.9.1
	github.com/robfig/cron v1.2.0
	github.com/sirupsen/logrus v1.6.0
	github.com/spf13/cobra v1.1.3
	github.com/stretchr/testify v1.6.1
	k8s.io/api v0.20.2
	k8s.io/apimachinery v0.20.2
	k8s.io/client-go v0.20.2
	sigs.k8s.io/controller-runtime v0.8.3
)
