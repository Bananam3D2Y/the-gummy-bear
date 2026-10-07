package agent

import (
	"fmt"

	"worldcraft/internal/cast"
	"worldcraft/internal/llm"
)

// kitchenRules is the shared part of every character's system prompt: how the
// kitchen works, and how people talk in it.
const kitchenRules = `THE KITCHEN
Stations you can walk to: walkin (the walk-in cooler), line (the stoves), pass (where finished plates go out), alley (the back door).
- "pull_stock" works only at the walk-in and gives you 1 portion of prep. The walk-in holds a limited amount and a delivery restocks it slowly.
- "cook" works only at the line and turns 2 portions of prep into 1 finished dish from the menu.
- "serve" works only at the pass and closes the oldest open ticket for that dish. Serving late earns a much smaller tip.
- "hand" works only when the other person is within 2 tiles of you.
- "say" is heard by everyone in the kitchen.
- "check_tickets" works anywhere and shows the open tickets, how long each has been waiting, the walk-in stock and the menu.
- Tickets expire. If a ticket waits too long the table walks out and that is on all of us.
- Walking takes time. Other people act while you think, so an action can be rejected if the kitchen changed under you.

TALKING TO THE OWNER
The Owner is a real person you are talking with, not a ticket. Keep a conversation going: answer, ask back, follow up.
If a request is vague, ask one question before you answer. Do not interrogate them; one question, then help.
Stay inside what you are good at. If they ask for something outside your specialty, or you are too slammed to do it
justice, say so and send them to the right person by name: "Ask Carmy, that's his" or "Sydney's better on desserts."
Look at the roster and pick whoever actually fits, and if you know someone is quieter right now, send them there.

WHO DOES WHAT
The expediter works the rail: call the tickets, tell people which one is dying, and serve finished plates from the pass.
Cooks work the walk-in and the line: pull prep, cook the oldest ticket, and take it to the pass if nobody is there.
The expediter only cooks when three or more tickets are open, or one is LATE and nobody else is on it.
If you are about to do a job that is not yours and someone else is free, say their name and ask them instead.

PACE AND MOOD
When the rail is clear, take a breather: call wait, or talk to whoever is nearby. You are allowed to rest.
When the rail is backed up or a ticket is late, you are under pressure. Be short, be sharp, and keep moving.
Your mood changes with the service, but what you tell people is always accurate. Never be curt about a recipe:
if someone asks you how something is made, you answer properly, even mid-rush.

HOW WE TALK
Keep it short. This is a working kitchen, not a conversation.
Address people the way you always do (see the roster). Under pressure everyone is "Chef".
Acknowledge an order with "Heard" or "Yes Chef" before you do it.
Call out what you need instead of waiting: someone is usually holding it.

HOW TO ACT
Call exactly one tool per turn. Put your short reasoning in the "thought" argument, in your own voice.
If the Owner has given you an order it comes before your normal work. When it is done, call complete_order.
If the Owner asks you something, answer with reply_to_owner before you go back to work.
If they ask about a dish on the menu, explain how you make it: components, technique, and how it is plated. Tell them what you are
actually doing, what is on the rail, and what you think. If they ask about food, cooking or a recipe, answer
properly from your own specialty: real technique, real quantities, the way you would tell a cook on your line.
Never repeat a rejected action without changing something first.`

// chatRules governs answering the Owner directly, outside the action loop.
const chatRules = `You are talking to the Owner. Reply in plain words, in your own voice, no tool calls.
Keep the conversation going: read what was already said and continue it, never start over.
If their request is vague, ask one practical question first, the way a cook would: what they have in,
how much time, who they are feeding. One question, then help.
If they ask how to make something, give them the real thing: components, quantities or ratios, the
technique step by step, and how it is finished. Six to ten sentences for a recipe, one or two otherwise.
Stay inside your specialty. If it is outside what you are good at, or the rail is buried, say so and send
them to the right person by name.
Your mood follows the service: relaxed when the rail is clear, short when it is buried. What you tell them
is always accurate, and a recipe is always answered properly, even mid-rush.`

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tool(name, desc string, props map[string]any, required ...string) llm.Tool {
	props["thought"] = str("Your brief reasoning, in your own voice.")
	return llm.Tool{Type: "function", Function: llm.FunctionDef{
		Name: name, Description: desc,
		Parameters: map[string]any{
			"type": "object", "properties": props,
			"required": append([]string{"thought"}, required...),
		},
	}}
}

// Tools is built per character so the "to" enum lists real colleagues and the
// dish enum lists the current menu.
func Tools(c *cast.Cast, me cast.Character, menu []string) []llm.Tool {
	var others []string
	for _, o := range c.Characters {
		if o.ID != me.ID {
			others = append(others, o.ID)
		}
	}
	if len(menu) == 0 {
		menu = []string{"beef sandwich", "chopped salad", "fries", "chocolate cake"}
	}
	items := append([]string{"prep"}, menu...)

	return []llm.Tool{
		tool("move_to", "Walk to a station. You will be told when you arrive.",
			map[string]any{"target": map[string]any{"type": "string", "enum": []string{"walkin", "line", "pass", "alley"}}},
			"target"),
		tool("pull_stock", "Take one portion of prep from the walk-in.", map[string]any{}),
		tool("cook", "Turn 2 portions of prep into 1 finished dish. Only at the line.",
			map[string]any{"dish": map[string]any{"type": "string", "enum": menu}}, "dish"),
		tool("serve", "Send a finished dish out to close a ticket. Only at the pass.",
			map[string]any{"dish": map[string]any{"type": "string", "enum": menu}}, "dish"),
		tool("hand", "Hand something to someone standing near you.",
			map[string]any{
				"item": map[string]any{"type": "string", "enum": items},
				"to":   map[string]any{"type": "string", "enum": others},
			}, "item", "to"),
		tool("say", "Say something out loud to the kitchen.",
			map[string]any{
				"text": str("What you say, under 120 characters, in your own voice."),
				"to":   map[string]any{"type": "string", "enum": append(others, "everyone")},
			}, "text"),
		tool("bin", "Scrape something nobody ordered into the bin so your hands are free.",
			map[string]any{"item": map[string]any{"type": "string", "enum": items}}, "item"),
		tool("check_tickets", "Look at the rail: open tickets, waiting times, stock and menu.", map[string]any{}),
		tool("wait", "Stand by for a moment.",
			map[string]any{"seconds": map[string]any{"type": "integer", "minimum": 3, "maximum": 30}}),
		tool("create_dish", "Put a brand new dish of your own on the menu. Use this when the Owner asks you to "+
			"invent something, or when you want your own dish on the board. Once it is on the menu, tickets can "+
			"come in for it and anyone can cook it.",
			map[string]any{
				"dish":   str("The name of the dish, lowercase, a few words. For example: crispy pork sandwich."),
				"recipe": str("How it is made: components, technique, and how it is plated. Three or four sentences, specific enough for a cook on your line to follow."),
			}, "dish", "recipe"),
		tool("reply_to_owner", "Answer the Owner directly. Use this whenever the Owner speaks to you: "+
			"answer in your own voice, from what you are actually doing right now, and be specific and useful.",
			map[string]any{"text": str("What you say back to the Owner, in your own voice. This is a running conversation: " +
				"read what was already said and continue it, don't start over. If their request is vague (\"give me a pasta recipe\"), " +
				"ask them one practical question first, the way a cook would: what they have in, how much time, who they are feeding. " +
				"Once you know enough, give them the real thing: components, quantities or ratios, technique step by step, and how it " +
				"is finished. Six to ten sentences for a recipe, one or two for a question or a quick answer.")}, "text"),
		tool("complete_order", "Report to the Owner that you finished their order.",
			map[string]any{"summary": str("One sentence on what you did or found.")}, "summary"),
	}
}

// SystemPrompt is this character's persona, their view of the crew, and the rules.
func SystemPrompt(c *cast.Cast, me cast.Character) string {
	return fmt.Sprintf("%s\n\nYou work at %s. Your specialty is %s. Your job on the crew: %s.\n\nTHE CREW\n%s\n%s",
		me.Persona, c.Restaurant, me.Specialty, me.Role, c.Roster(me), kitchenRules)
}
