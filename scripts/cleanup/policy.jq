# Accept provider RFC3339 timestamps, including fractional seconds and offsets.
def epoch:
  if type == "number" then .
  elif type != "string" then 0
  elif test("^[0-9]+$") then tonumber
  else (try (
    capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<offset>Z|[+-][0-9]{2}:?[0-9]{2})$") |
    (.base + "Z" | fromdateiso8601) -
    (if .offset == "Z" then 0 else
      (.offset | gsub(":"; "")) as $offset |
      (($offset[1:3] | tonumber) * 3600 + ($offset[3:5] | tonumber) * 60) *
      (if $offset[0:1] == "-" then -1 else 1 end)
    end)
  ) catch 0) // 0 end;

def old($cutoff): epoch | . > 0 and . <= $cutoff;
def test_cluster: test("^gcp-chart-testing-cluster-[0-9]+-[a-z0-9]{6}$");
def test_infra: (try capture("^(?<infra>ocp-[a-f0-9]{10}-[a-z0-9]{5})(?:-|$)").infra catch null) // null;

def protected:
  (.name // .infraID // "") as $name |
  ((env.CLEANUP_PROTECTED_RESOURCES // "" | [splits("[,\\s]+") | select(length > 0)]) as $names |
    any($names[]; . as $prefix | $name == $prefix or ($name | startswith($prefix + "-")))) or
  ((.labels["cleanup-retain"] // .resourceLabels["cleanup-retain"] // .tags.CleanupRetain // "false" | tostring | ascii_downcase) == "true");
