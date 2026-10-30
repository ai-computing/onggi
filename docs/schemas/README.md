# Schemas of the device binding interface

JSON Schema (draft 2020-12) of the documents exchanged between onggi and a device binding
([ADR-0010](../adr/0010-device-binding-contract.md)).

| schema | file | produced by | example |
|---|---|---|---|
| `onggi.switchplan.v1` | [switchplan.v1.schema.json](switchplan.v1.schema.json) | `onggi diff --ranges --json BASE TARGET` | [examples/switchplan.json](examples/switchplan.json) |
| `onggi.updateplan.v1` | [updateplan.v1.schema.json](updateplan.v1.schema.json) | binding `plan` | [examples/updateplan.json](examples/updateplan.json), [examples/updateplan-reload.json](examples/updateplan-reload.json) |
| `onggi.verification.v1` | [verification.v1.schema.json](verification.v1.schema.json) | binding `apply`, `rollback` | [examples/verification.json](examples/verification.json) |
| `onggi.resident.v1` | [resident.v1.schema.json](resident.v1.schema.json) | binding `resident` | [examples/resident.json](examples/resident.json) |
| `onggi.profile.v1` | `store.Profile` (#71/#72) | publisher, binding `describe` | [examples/profile.json](examples/profile.json) |

The examples come from the `inplace` and `rearranged` golden switches of
[`conformance/binding`](../../conformance/binding/) and the reference fake binding. To validate them (needs the
`jsonschema` package, not used by onggi itself):

```bash
python3 -c 'import json,glob,jsonschema; from referencing import Registry, Resource
s={json.load(open(p))["$id"]:json.load(open(p)) for p in glob.glob("docs/schemas/*.schema.json")}
r=Registry().with_resources([(k,Resource.from_contents(v)) for k,v in s.items()]); t={v["title"]:v for v in s.values()}
[jsonschema.Draft202012Validator(t[d["schema"]],registry=r).validate(d) for p in glob.glob("docs/schemas/examples/*.json")+glob.glob("conformance/binding/golden/*.json") for d in [json.load(open(p))] if d["schema"] in t]'
```
