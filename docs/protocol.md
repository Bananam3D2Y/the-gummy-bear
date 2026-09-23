# WorldCraft message protocol

All messages are JSON. Kafka keys are the agent ID (`miner`, `blacksmith`, `merchant`) except
`world.snapshots`, which uses the key `world`.

| Topic | Producer | Consumer(s) | Partitions |
|---|---|---|---|
| `world.snapshots` | engine | gateway, brain (world view) | 1 |
| `world.events` | engine | brain (one group per agent), gateway | 3 |
| `agent.actions` | brain | engine | 3 |
| `overseer.commands` | gateway | brain | 3 |
| `agent.thoughts` | brain | gateway | 3 |

## Actions (brain -> engine, `agent.actions`)

```json
{"type":"move_to","agent_id":"miner","target":"forge","based_on_tick":4120,"decision_id":"miner-1726"}
{"type":"mine","agent_id":"miner","based_on_tick":4120}
{"type":"craft","agent_id":"blacksmith","item":"tool","based_on_tick":4120}
{"type":"give","agent_id":"miner","item":"ore","to":"blacksmith","based_on_tick":4120}
{"type":"sell","agent_id":"merchant","item":"tool","based_on_tick":4120}
{"type":"say","agent_id":"merchant","text":"Tools are selling high!","to":"blacksmith","based_on_tick":4120}
{"type":"check_market","agent_id":"merchant","based_on_tick":4120}
```

`based_on_tick` is the world tick the agent was looking at when it decided. The engine measures
`current_tick - based_on_tick` as staleness.

## Events (engine -> brain, `world.events`)

Every event carries `type`, `agent_id` (the agent who perceives it), `tick`, and `state`:

```json
{"type":"arrived","agent_id":"miner","tick":4200,"location":"forge",
 "state":{"x":22,"y":15,"location":"forge","inventory":{"ore":2,"tool":0},"coins":10,"moving":false,"destination":""}}
```

| type | extra fields |
|---|---|
| `arrived` | `location` |
| `action_result` | `action`, `detail` |
| `action_rejected` | `action`, `reason`, `staleness_ticks` |
| `heard` | `from`, `to`, `text` |
| `received` | `from`, `item` |
| `market_report` | `prices` {item: price}, `history` {item: [prices]} |

Rejection reasons: `unknown_location`, `no_path`, `blocked`, `not_at_mine`, `mine_empty`,
`unknown_recipe`, `not_at_forge`, `not_enough_ore`, `unknown_agent`, `item_missing`,
`target_not_nearby`, `unknown_item`, `not_at_market`, `empty_text`, `unknown_action`.

## Overseer commands (gateway -> brain, `overseer.commands`)

```json
{"agent_id":"blacksmith","text":"Stop forging and analyze the market","ts":1726990000000}
```
`agent_id` may be `all`.

## Thoughts (brain -> gateway, `agent.thoughts`)

```json
{"agent_id":"merchant","text":"Tool price is rising, hold stock.","tool":"wait","args":"{\"seconds\":10}",
 "tick":4300,"model":"gemini-2.5-flash-lite","latency_ms":1840,"retrieved":4,"ts":1726990000000}
```

## Snapshot (engine -> gateway, `world.snapshots`, 10 Hz)

```json
{"type":"snapshot","tick":4300,"width":40,"height":30,"tiles":["####...", "..."],
 "landmarks":{"mine":[6,4],"forge":[22,14],"market":[33,22],"square":[20,20]},
 "mine_stock":4,"prices":{"ore":4.8,"tool":21.3},
 "agents":[{"id":"miner","role":"miner","x":6,"y":6,"location":"mine","destination":"",
            "inventory":{"ore":1,"tool":0},"coins":10,"bubble":"","moving":false}]}
```
