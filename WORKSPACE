workspace(name = "com_github_coachroachdb_helm_charts")

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "io_bazel_rules_go",
    sha256 = "8e968b5fcea1d2d64071872b12737bbb5514524ee5f0a4f54f5920266c261acb",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/rules_go/releases/download/v0.28.0/rules_go-v0.28.0.zip",
        "https://github.com/bazelbuild/rules_go/releases/download/v0.28.0/rules_go-v0.28.0.zip",
    ],
)

http_archive(
    name = "bazel_gazelle",
    sha256 = "62ca106be173579c0a167deb23358fdfe71ffa1e4cfdddf5582af26520f1c66f",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/bazel-gazelle/releases/download/v0.23.0/bazel-gazelle-v0.23.0.tar.gz",
        "https://github.com/bazelbuild/bazel-gazelle/releases/download/v0.23.0/bazel-gazelle-v0.23.0.tar.gz",
    ],
)

load("@io_bazel_rules_go//go:deps.bzl", "go_register_toolchains", "go_rules_dependencies")
load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies", "go_repository")

go_rules_dependencies()
go_register_toolchains(version = "1.18")
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
