module github.com/william-lbn/control-plane/api

go 1.26.0

toolchain go1.27.1

require (
	// 同步主线OIDC/OAuth2依赖，同时保留DSGA的Kubernetes/JSON Patch依赖。
	// OAuth2使用主线0.36.0，不能在合并冲突时退回旧间接依赖0.27.0。
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/evanphx/json-patch/v5 v5.9.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pglogrepl v0.0.0-20251213150135-2e8d0df862c1
	github.com/jackc/pgx/v5 v5.9.2
	github.com/minio/minio-go/v7 v7.2.0
	github.com/pquerna/otp v1.5.0
	golang.org/x/crypto v0.56.0
	golang.org/x/net v0.57.0
	golang.org/x/oauth2 v0.36.0
	k8s.io/api v0.31.7
	k8s.io/apimachinery v0.31.7
	k8s.io/client-go v0.31.7
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/emicklei/go-restful/v3 v3.12.1 // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/fxamacker/cbor/v2 v2.7.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/gnostic-models v0.6.9-0.20230804172637-c7be7c783f49 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/imdario/mergo v0.3.16 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/moby/sys/mountinfo v0.7.1 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runc v1.1.13 // indirect
	github.com/opencontainers/runtime-spec v1.0.3-0.20220909204839-494a5a6aca78 // indirect
	github.com/opencontainers/selinux v1.11.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.20.4 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/time v0.6.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.4.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240903143218-8af14fe29dc1 // indirect
	google.golang.org/grpc v1.66.2 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.12.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apiextensions-apiserver v0.31.1 // indirect
	k8s.io/apiserver v0.31.7 // indirect
	k8s.io/component-base v0.31.7 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
	k8s.io/kube-openapi v0.0.0-20240903163716-9e1beecbcb38 // indirect
	k8s.io/kubernetes v1.31.7 // indirect
	k8s.io/mount-utils v0.0.0 // indirect
	k8s.io/utils v0.0.0-20240921022957-49e7df575cb6 // indirect
	sigs.k8s.io/controller-runtime v0.19.7 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.4.1 // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)

require (
	github.com/boombuler/barcode v1.0.1-0.20190219062509-6c824513bacc // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgio v1.0.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	// 主线CRC64现在直接引用；保留其依赖性质，同时保留下方DSGA准确fork替换。
	github.com/minio/crc64nvme v1.1.1
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/neondatabase/autoscaling v0.0.0-20261020225115-3eeafd997c8c
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/xdg-go/stringprep v1.0.4
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0
	gopkg.in/ini.v1 v1.67.2 // indirect
)

replace github.com/neondatabase/autoscaling => github.com/william-lbn/autoscaling v0.0.0-20261124060455-3567f1c5076e

replace k8s.io/mount-utils => k8s.io/mount-utils v0.31.7

replace k8s.io/api => k8s.io/api v0.31.7

replace k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.31.7

replace k8s.io/apimachinery => k8s.io/apimachinery v0.31.7

replace k8s.io/apiserver => k8s.io/apiserver v0.31.7

replace k8s.io/cli-runtime => k8s.io/cli-runtime v0.31.7

replace k8s.io/client-go => k8s.io/client-go v0.31.7

replace k8s.io/cloud-provider => k8s.io/cloud-provider v0.31.7

replace k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.31.7

replace k8s.io/code-generator => k8s.io/code-generator v0.31.7

replace k8s.io/component-base => k8s.io/component-base v0.31.7

replace k8s.io/component-helpers => k8s.io/component-helpers v0.31.7

replace k8s.io/controller-manager => k8s.io/controller-manager v0.31.7

replace k8s.io/cri-api => k8s.io/cri-api v0.31.7

replace k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.31.7

replace k8s.io/dynamic-resource-allocation => k8s.io/dynamic-resource-allocation v0.31.7

replace k8s.io/endpointslice => k8s.io/endpointslice v0.31.7

replace k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.31.7

replace k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.31.7

replace k8s.io/kube-proxy => k8s.io/kube-proxy v0.31.7

replace k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.31.7

replace k8s.io/kubectl => k8s.io/kubectl v0.31.7

replace k8s.io/kubelet => k8s.io/kubelet v0.31.7

replace k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.31.7

replace k8s.io/metrics => k8s.io/metrics v0.31.7

replace k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.31.7

replace k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.31.7

replace k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.31.7

replace k8s.io/sample-controller => k8s.io/sample-controller v0.31.7
