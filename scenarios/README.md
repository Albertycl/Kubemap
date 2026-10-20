# Scenarios (KM-82)

Each file is one real question an SRE gets, walked through the way a senior engineer checks. The portal embeds every `scenarios/<area>/<id>.yaml` and shows it at `#/scenarios`.

## Rules the loader enforces (`go test ./scenarios`, so `make all` fails on a bad file)
- `id` is lowercase words joined by `-`, equal to the file name; `area` is `traffic`, `pods`, `nodes`, `security` or `operations` (rollouts through Git, upgrades, incident response), equal to the folder. IDs are unique.
- `title`, `question` and at least one step are required; every step needs `title`, `why` and `command`.
- Unknown fields are errors (a typo such as `commnad:` fails).
- `kubemap` (the page a step opens) is one of `app-map`, `pod`, `node`, `netpol`, `logs`, `findings`, `debug`.
- `check.op` must be on the read-only allow list, with its arguments:

| op | arguments |
|---|---|
| `service.check` | `namespace`, `service` (both optional) |
| `pod.debug`, `pod.diagnose` | `namespace`, `pod` |
| `node.pressure` | `node` (optional) |
| `health.check` | `namespace` (optional) |
| `appmap` | `namespace`, `app` (optional) |
| `netpol.check` | `fromNamespace`, `fromPod`, `toNamespace`, `port`, and exactly one of `toService`, `toPod`; `protocol` optional |

- Placeholders: `$name` in check arguments, `<name>` in commands. Known names: `ns`, `svc`, `pod`, `container`, `node`, `app`, `port`, `from`, `fromns`, `deploy`, `pvc`, `hpa`, `cert`, `argoapp`, `host` (object and host names, checked like `ns`). The page asks for the ones a scenario uses, and a start link fills them from a real object.
- `params` is computed from the placeholders; do not write it.
- Placeholder values are checked by kind before they are used anywhere (review KM-82 B1): names must be DNS names, `app` is `namespace/name`, `port` is 1–65535. An invalid value is never put into a command (the `<name>` stays), is never sent to an agent, and the field says why. Commands end up in Copy boxes that people paste into a shell, so this rule must hold for every new placeholder too: add its rule to `ValidValue` (Go) and `invalidValue` (web) together.

## Writing a good one
- Order the steps as you would check them under pressure: the one check that splits the problem in two comes first.
- `why` explains the mechanism, not the command. `good` and `bad` say what the reader sees and where to go next.
- Every cause has a fix and a command that proves the fix (`prove`).
- `senior` is what an interviewer wants to hear: blast radius, SLO impact, mitigation before root cause, what to automate.
