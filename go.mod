module github.com/goccy/kubetest

go 1.16

require (
	cloud.google.com/go v0.110.7 // indirect
	github.com/bradleyfalzon/ghinstallation/v2 v2.0.4
	github.com/go-git/go-billy/v5 v5.3.1
	github.com/go-git/go-git/v5 v5.4.2
	github.com/go-logr/logr v1.2.4
	github.com/goccy/kubejob v0.3.3
	github.com/google/go-github/v29 v29.0.2
	github.com/jessevdk/go-flags v1.5.0
	github.com/lestrrat-go/backoff v1.0.1
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.27.6
	github.com/sosedoff/gitkit v0.3.0
	golang.org/x/sync v0.2.0
	k8s.io/api v0.28.0
	k8s.io/apimachinery v0.28.0
	k8s.io/client-go v0.28.0
	k8s.io/component-base v0.21.0 // indirect
	sigs.k8s.io/controller-runtime v0.8.3
)

replace github.com/goccy/kubejob => github.com/zoncoen/kubejob v0.0.0-20230816174056-9ef94255d1aa
