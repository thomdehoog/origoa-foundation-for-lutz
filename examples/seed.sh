#!/usr/bin/env bash
# Seeds a running origoad (default http://127.0.0.1:8080) with a small
# requirements-management domain: schemas, a workflow, entries, an overlay,
# a document, links and a comment.
set -euo pipefail
API=${1:-http://127.0.0.1:8080}/api

post() { curl -sfS -X POST "$API/$1" -H 'Content-Type: application/json' -d "$2"; }
put()  { curl -sfS -X PUT  "$API/$1" -H 'Content-Type: application/json' -d "$2"; }
guid() { python3 -c 'import json,sys; print(json.load(sys.stdin)["meta"]["guid"])'; }

put "workflows/dev" '{"id":"dev","initial":"open","states":["open","review","done"],
  "transitions":[{"from":"open","to":"review"},{"from":"review","to":"done"},{"from":"review","to":"open"}]}' >/dev/null

put "schemas/requirement" '{"artifactType":"requirement","kind":"entry","displayName":"Requirement",
  "hidPrefix":"REQ","workflows":["dev"],
  "fields":[{"id":"priority","name":"Priority","type":"enum","options":["low","medium","high"]},
            {"id":"rationale","name":"Rationale","type":"text"}],
  "relationships":[{"linkType":"verifies","targetTypes":["testcase"],"cardinality":"many-to-many"}]}' >/dev/null

put "schemas/testcase" '{"artifactType":"testcase","kind":"entry","displayName":"Test Case","hidPrefix":"TC"}' >/dev/null
put "schemas/spec" '{"artifactType":"spec","kind":"document","displayName":"Specification"}' >/dev/null

R1=$(post entries '{"path":"specs/boot","type":"requirement","title":"System boots in under 2 seconds",
  "fields":{"priority":"high","rationale":"Startup latency is a key selling point."}}' | guid)
R2=$(post entries '{"path":"specs/boot","type":"requirement","title":"Boot progress is displayed",
  "fields":{"priority":"low"}}' | guid)
V1=$(post entries "{\"path\":\"specs/boot\",\"type\":\"requirement\",\"title\":\"Variant: embedded boot\",
  \"base\":\"$R1\",\"fields\":{\"priority\":\"medium\"}}" | guid)
T1=$(post entries '{"path":"tests","type":"testcase","title":"Measure cold boot time"}' | guid)

post links "{\"type\":\"verifies\",\"source\":\"$R1\",\"target\":\"$T1\"}" >/dev/null
post comments "{\"subject\":\"$R1\",\"author\":\"seed\",\"text\":\"Is 2s realistic on the low-end SKU?\"}" >/dev/null
post documents "{\"path\":\"docs\",\"type\":\"spec\",\"title\":\"Boot Specification\",
  \"content\":[{\"type\":\"section\",\"title\":\"Timing\",\"children\":[
    {\"type\":\"text\",\"text\":\"The following requirements apply:\"},
    {\"type\":\"entryRef\",\"guid\":\"$R1\"},{\"type\":\"entryRef\",\"guid\":\"$R2\"}]}]}" >/dev/null
post "artifacts/$R1/transition" '{"workflow":"dev","to":"review"}' >/dev/null

echo "Seeded: requirements $R1, $R2, overlay $V1, testcase $T1"
