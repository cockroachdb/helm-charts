workspace(name = "com_github_coachroachdb_helm_charts")

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "io_bazel_rules_go",
    sha256 = "90fe8fb402dee957a375f3eb8511455bd738c7ed562695f4dd117ac7d2d833b1",
    urls = [
        "https://mirror.bazel.build/github.com/bazel-contrib/rules_go/releases/download/v0.52.0/rules_go-v0.52.0.zip",
        "https://github.com/bazel-contrib/rules_go/releases/download/v0.52.0/rules_go-v0.52.0.zip",
    ],
)

http_archive(
    name = "bazel_gazelle",
    integrity = "sha256-12v3pg/YsFBEQJDfooN6Tq+YKeEWVhjuNdzspcvfWNU=",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/bazel-gazelle/releases/download/v0.37.0/bazel-gazelle-v0.37.0.tar.gz",
        "https://github.com/bazelbuild/bazel-gazelle/releases/download/v0.37.0/bazel-gazelle-v0.37.0.tar.gz",
    ],
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies", "go_repository")
load("@io_bazel_rules_go//go:deps.bzl", "go_register_toolchains", "go_rules_dependencies")

go_rules_dependencies()

go_register_toolchains(version = "1.23.4")

gazelle_dependencies()

go_repository(
    name = "in_gopkg_yaml_v3",
    build_file_proto_mode = "disable_global",
    importpath = "gopkg.in/yaml.v3",
    sha256 = "5169b5625d3c351f13e8a4ec4802f709072701b441ed92181c6051ece53615a9",
    strip_prefix = "gopkg.in/yaml.v3@v3.0.0-20210107192922-496545a6307b",
    urls = [
        "https://storage.googleapis.com/cockroach-godeps/gomod/gopkg.in/yaml.v3/in_gopkg_yaml_v3-v3.0.0-20210107192922-496545a6307b.zip",
    ],
)

go_repository(
    name = "com_github_masterminds_semver_v3",
    build_file_proto_mode = "disable_global",
    importpath = "github.com/Masterminds/semver/v3",
    sha256 = "0a46c7403dfeda09b0821e851f8e1cec8f1ea4276281e42ea399da5bc5bf0704",
    strip_prefix = "github.com/Masterminds/semver/v3@v3.1.1",
    urls = [
        "https://storage.googleapis.com/cockroach-godeps/gomod/github.com/Masterminds/semver/v3/com_github_masterminds_semver_v3-v3.1.1.zip",
    ],
)
