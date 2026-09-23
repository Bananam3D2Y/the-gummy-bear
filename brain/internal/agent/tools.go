package agent

import "worldcraft/internal/llm"

// Persona is the fixed part of each agent's system prompt.
var Personas = map[string]string{
	"miner": "You are Tove, the village miner. You dig ore at the mine and bring it to the blacksmith at the forge. " +
		"You are practical and a little impatient.",
	"blacksmith": "You are Brann, the village blacksmith. You turn ore into tools at the forge and hand finished tools " +
		"to the merchant, or sell them yourself if the price is good. You are proud of your work.",
	"merchant": "You are Wren, the village merchant. You sell tools at the market for coins and keep an eye on prices. " +
		"You are chatty and always looking for a bargain.",
}

const worldRules = `THE VILLAGE
Places you can walk to: mine, forge, market, square.
- "mine" works only at the mine; the mine holds a limited amount of ore that slowly refills.
- "craft" works only at the forge and turns 2 ore into 1 tool.
- "sell" works only at the market; selling pushes that item's price down a little.
- "give" works only when the other agent is within 2 tiles of you.
- "say" is heard by everyone within 10 tiles.
- "check_market" works anywhere and returns current prices plus recent price history.
- Walking takes time. Other villagers act while you think, so an action can be rejected if the world changed.

YOUR LANE
Tove the miner mines ore. She never crafts and never sells. She gives ore to Brann.
Brann the blacksmith crafts tools from ore. He never mines. He gives finished tools to Wren.
Wren the merchant sells tools at the market. She never mines and never crafts.
If you need something outside your lane, walk to that villager and ask for it with "say".
Do not hoard: hand items to the next villager in the chain as soon as you have them.

HOW TO ACT
Call exactly one tool per turn. Put your short private reasoning in the "thought" argument (one or two sentences).
If the Overseer has given you an order, it takes priority over your normal routine. When the order is done, call complete_order.
Never repeat a rejected action without changing something first.`

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tool(name, desc string, props map[string]any, required ...string) llm.Tool {
	props["thought"] = str("Your brief reasoning for this action.")
	return llm.Tool{Type: "function", Function: llm.FunctionDef{
		Name: name, Description: desc,
		Parameters: map[string]any{
			"type": "object", "properties": props,
			"required": append([]string{"thought"}, required...),
		},
	}}
}

var Tools = []llm.Tool{
	tool("move_to", "Walk to a named place. You will be told when you arrive.",
		map[string]any{"target": map[string]any{"type": "string", "enum": []string{"mine", "forge", "market", "square"}}},
		"target"),
	tool("mine", "Dig one ore. Only works at the mine.", map[string]any{}),
	tool("craft", "Turn 2 ore into 1 tool. Only works at the forge.",
		map[string]any{"item": map[string]any{"type": "string", "enum": []string{"tool"}}}, "item"),
	tool("give", "Hand one item to a nearby villager.",
		map[string]any{
			"item": map[string]any{"type": "string", "enum": []string{"ore", "tool"}},
			"to":   map[string]any{"type": "string", "enum": []string{"miner", "blacksmith", "merchant"}},
		}, "item", "to"),
	tool("sell", "Sell one item at the market for coins.",
		map[string]any{"item": map[string]any{"type": "string", "enum": []string{"ore", "tool"}}}, "item"),
	tool("say", "Say something out loud to nearby villagers.",
		map[string]any{
			"text": str("What you say, under 120 characters."),
			"to":   map[string]any{"type": "string", "enum": []string{"miner", "blacksmith", "merchant", "everyone"}},
		}, "text"),
	tool("check_market", "Look at current market prices and their recent history.", map[string]any{}),
	tool("wait", "Do nothing for a while.",
		map[string]any{"seconds": map[string]any{"type": "integer", "minimum": 3, "maximum": 30}}),
	tool("complete_order", "Report that you finished the Overseer's order.",
		map[string]any{"summary": str("One sentence on what you did or found.")}, "summary"),
}
