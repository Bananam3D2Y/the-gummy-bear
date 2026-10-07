# WorldCraft Kitchen message protocol

All messages are JSON. Kafka keys are the character ID from `cast.json`
(`carmy`, `sydney`, `richie`, `tina`, `marcus`, `ebraheim`, `fak`), except
`world.snapshots`, which uses the key `world`.

| Topic | Producer | Consumer(s) | Partitions |
|---|---|---|---|
| `world.snapshots` | engine | gateway, brain (world view) | 1 |
| `world.events` | engine | brain (one group per character), gateway | 3 |
| `agent.actions` | brain | engine | 3 |
| `overseer.commands` | gateway | brain | 3 |
| `agent.thoughts` | brain | gateway | 3 |

## Actions (brain -> engine, `agent.actions`)

```json
{"type":"move_to","agent_id":"tina","target":"line","based_on_tick":4120,"decision_id":"tina-1726"}
{"type":"pull_stock","agent_id":"fak","based_on_tick":4120}
{"type":"cook","agent_id":"tina","dish":"beef sandwich","based_on_tick":4120}
{"type":"serve","agent_id":"carmy","dish":"beef sandwich","based_on_tick":4120}
{"type":"hand","agent_id":"fak","item":"prep","to":"tina","based_on_tick":4120}
{"type":"say","agent_id":"richie","text":"Cousin, two on the rail!","to":"carmy","based_on_tick":4120}
{"type":"check_tickets","agent_id":"richie","based_on_tick":4120}
```

Stations: `walkin`, `line`, `pass`, `alley`. Items: `prep` plus any dish on the menu.
`based_on_tick` is the tick the character was looking at when it decided; the engine
measures `current_tick - based_on_tick` as staleness.

## Events (engine -> brain, `world.events`)

Every event carries `type`, `agent_id` (who perceives it), `tick`, and `state`:

```json
{"type":"arrived","agent_id":"tina","tick":4200,"location":"line",
 "state":{"x":20,"y":14,"location":"line","inventory":{"prep":2},"coins":10,"moving":false,"destination":""}}
```

| type | extra fields | who gets it |
|---|---|---|
| `arrived` | `location` | the character |
| `action_result` | `action`, `detail` | the character |
| `action_rejected` | `action`, `reason`, `staleness_ticks` | the character |
| `heard` | `from`, `to`, `text` | everyone within 12 tiles |
| `received` | `from`, `item` | the recipient |
| `ticket_in` | `ticket_id`, `dish`, `open_tickets` | the expediter only |
| `walkout` | `ticket_id`, `dish`, `walkouts` | the expediter only |
| `ticket_report` | `tickets[]`, `stock`, `menu`, `served`, `walkouts` | whoever called `check_tickets` |

Ticket news goes only to the expediter so one new ticket costs one LLM call, not seven.

Rejection reasons: `unknown_station`, `no_path`, `blocked`, `not_at_walkin`, `out_of_stock`,
`not_on_menu`, `not_at_line`, `not_enough_prep`, `not_at_pass`, `dish_missing`,
`no_ticket_for_dish`, `unknown_agent`, `item_missing`, `target_not_nearby`, `empty_text`,
`unknown_action`.

## Orders from the Owner (gateway -> brain, `overseer.commands`)

```json
{"agent_id":"carmy","text":"Clear the two oldest tickets before anything else","ts":1726990000000}
```
`agent_id` may be `all`.

## Thoughts (brain -> gateway, `agent.thoughts`)

```json
{"agent_id":"richie","text":"Rail's backing up, someone needs to fire the salad.","tool":"say",
 "args":"{\"text\":\"Cousin, two on the rail!\",\"to\":\"carmy\"}","tick":4300,
 "model":"gemini-3.5-flash-lite","latency_ms":1840,"retrieved":4,"ts":1726990000000}
```

## Snapshot (engine -> gateway, `world.snapshots`, 10 Hz)

```json
{"type":"snapshot","tick":4300,"restaurant":"The Original Beef","width":40,"height":30,
 "tiles":["####...","..."],
 "landmarks":{"walkin":[5,6],"line":[20,13],"pass":[20,19],"alley":[34,5]},
 "stock":7,"served":12,"walkouts":1,
 "menu":["beef sandwich","chopped salad","fries","chocolate cake"],
 "tickets":[{"id":9,"dish":"fries","waiting_s":41,"due_in_s":49}],
 "agents":[{"id":"tina","name":"Tina","role":"line cook","color":"#8FBF6A","x":20,"y":14,
            "location":"line","destination":"","inventory":{"prep":2},"coins":30,
            "bubble":"","moving":false}]}
```

Names and colors come from `cast.json` via the engine, so the frontend never hardcodes the cast.
