#include "world.hpp"

#include <algorithm>
#include <cmath>
#include <fstream>

namespace {
const char* kEventsTopic = "world.events";

double round2(double v) { return std::round(v * 100.0) / 100.0; }
}  // namespace

// ---------------------------------------------------------------- setup

bool World::load_map(const std::string& path, std::string& err) {
  std::ifstream in(path);
  if (!in) {
    err = "cannot open map file: " + path;
    return false;
  }
  std::vector<std::string> rows;
  for (std::string line; std::getline(in, line);) {
    if (!line.empty() && line.back() == '\r') line.pop_back();
    if (!line.empty()) rows.push_back(line);
  }
  if (!grid_.load(rows)) {
    err = "map is empty or not rectangular";
    return false;
  }
  // Letters mark landmarks, digits mark agent spawn points.
  for (int y = 0; y < grid_.height; ++y) {
    for (int x = 0; x < grid_.width; ++x) {
      switch (grid_.at(x, y)) {
        case 'M': landmarks_["mine"] = {x, y}; break;
        case 'F': landmarks_["forge"] = {x, y}; break;
        case 'K': landmarks_["market"] = {x, y}; break;
        case 'S': landmarks_["square"] = {x, y}; break;
        case '1': case '2': case '3': spawns_[grid_.at(x, y)] = {x, y}; break;
        default: break;
      }
    }
  }
  for (const char* required : {"mine", "forge", "market", "square"}) {
    if (!landmarks_.count(required)) {
      err = std::string("map is missing landmark: ") + required;
      return false;
    }
  }
  for (auto& [name, _] : prices_) price_history_[name].push_back(prices_[name]);
  return true;
}

void World::spawn_agents() {
  struct Def { const char* id; const char* role; char spawn; };
  const Def defs[] = {{"miner", "miner", '1'}, {"blacksmith", "blacksmith", '2'},
                      {"merchant", "merchant", '3'}};
  for (const auto& d : defs) {
    Agent a;
    a.id = d.id;
    a.role = d.role;
    a.pos = spawns_.count(d.spawn) ? spawns_[d.spawn] : landmarks_["square"];
    a.coins = 10;
    agents_.push_back(a);
  }
}

// ---------------------------------------------------------------- helpers

Agent* World::find_agent(const std::string& id) {
  for (auto& a : agents_)
    if (a.id == id) return &a;
  return nullptr;
}

std::string World::location_of(const Agent& a) const {
  for (const auto& [name, p] : landmarks_)
    if (chebyshev(a.pos, p) <= kAtLocationRadius) return name;
  return "";
}

bool World::occupied_by_other(Point p, const Agent& self) const {
  for (const auto& o : agents_)
    if (o.id != self.id && o.pos == p) return true;
  return false;
}

std::unordered_set<int> World::other_agent_tiles(const Agent& self) const {
  std::unordered_set<int> s;
  for (const auto& o : agents_)
    if (o.id != self.id) s.insert(grid_.index(o.pos.x, o.pos.y));
  return s;
}

json World::state_of(const Agent& a) const {
  return {{"x", a.pos.x},
          {"y", a.pos.y},
          {"location", location_of(a)},
          {"inventory", a.inventory},
          {"coins", a.coins},
          {"moving", !a.path.empty()},
          {"destination", a.destination}};
}

// Every event carries the agent's current state so the brain never has to ask for it.
void World::emit(const Agent& a, const std::string& type, json extra) {
  extra["type"] = type;
  extra["agent_id"] = a.id;
  extra["tick"] = tick_;
  extra["state"] = state_of(a);
  outbox_.push_back({kEventsTopic, a.id, extra.dump()});
}

std::vector<OutMessage> World::drain_outbox() {
  std::vector<OutMessage> out;
  out.swap(outbox_);
  return out;
}

// ---------------------------------------------------------------- actions

// The engine is the referee: brains only REQUEST actions, and every request is
// validated against the world as it is NOW, not as it was when the LLM decided.
ActionOutcome World::apply_action(const json& action) {
  ActionOutcome out;
  out.type = action.value("type", std::string("unknown"));
  const std::string agent_id = action.value("agent_id", std::string());
  const long based_on = action.value("based_on_tick", tick_);
  out.staleness_ticks = std::max(0L, tick_ - based_on);

  Agent* ag = find_agent(agent_id);
  if (!ag) {
    out.reason = "unknown_agent";
    return out;
  }

  auto reject = [&](const std::string& reason) {
    emit(*ag, "action_rejected", {{"action", out.type}, {"reason", reason},
                                  {"staleness_ticks", out.staleness_ticks}});
    out.accepted = false;
    out.reason = reason;
    return out;
  };
  auto accept = [&](const std::string& detail) {
    if (!detail.empty()) emit(*ag, "action_result", {{"action", out.type}, {"detail", detail}});
    out.accepted = true;
    return out;
  };

  const std::string here = location_of(*ag);

  if (out.type == "move_to") {
    const std::string target = action.value("target", std::string());
    auto it = landmarks_.find(target);
    if (it == landmarks_.end()) return reject("unknown_location");
    if (here == target) {
      ag->path.clear();
      ag->destination.clear();
      emit(*ag, "arrived", {{"location", target}});
      return accept("");
    }
    auto route = astar(grid_, ag->pos, it->second, nullptr);
    if (route.empty()) return reject("no_path");
    ag->path.assign(route.begin(), route.end());
    ag->goal = it->second;
    ag->destination = target;
    ag->blocked_ticks = 0;
    return accept("");  // no event now; "arrived" fires when the walk finishes
  }

  if (out.type == "mine") {
    if (here != "mine") return reject("not_at_mine");
    if (mine_stock_ <= 0) return reject("mine_empty");
    --mine_stock_;
    ++ag->inventory["ore"];
    return accept("mined 1 ore (mine has " + std::to_string(mine_stock_) + " left)");
  }

  if (out.type == "craft") {
    const std::string item = action.value("item", std::string("tool"));
    if (item != "tool") return reject("unknown_recipe");
    if (here != "forge") return reject("not_at_forge");
    if (ag->inventory["ore"] < 2) return reject("not_enough_ore");
    ag->inventory["ore"] -= 2;
    ++ag->inventory["tool"];
    return accept("crafted 1 tool from 2 ore");
  }

  if (out.type == "give") {
    const std::string item = action.value("item", std::string());
    const std::string to = action.value("to", std::string());
    Agent* rec = find_agent(to);
    if (!rec || rec == ag) return reject("unknown_agent");
    if (ag->inventory[item] < 1) return reject("item_missing");
    if (chebyshev(ag->pos, rec->pos) > kAtLocationRadius) return reject("target_not_nearby");
    --ag->inventory[item];
    ++rec->inventory[item];
    emit(*rec, "received", {{"from", ag->id}, {"item", item}});
    return accept("gave 1 " + item + " to " + to);
  }

  if (out.type == "sell") {
    const std::string item = action.value("item", std::string());
    if (!prices_.count(item)) return reject("unknown_item");
    if (here != "market") return reject("not_at_market");
    if (ag->inventory[item] < 1) return reject("item_missing");
    const int earned = static_cast<int>(std::lround(prices_[item]));
    --ag->inventory[item];
    ag->coins += earned;
    prices_[item] = std::max(1.0, prices_[item] * 0.93);  // selling floods the market a little
    return accept("sold 1 " + item + " for " + std::to_string(earned) + " coins");
  }

  if (out.type == "say") {
    std::string text = action.value("text", std::string());
    if (text.empty()) return reject("empty_text");
    if (text.size() > 200) text.resize(200);
    const std::string to = action.value("to", std::string());
    ag->bubble = text;
    ag->bubble_ttl = kBubbleTicks;
    // Spatial query: only agents within hearing range receive the speech.
    for (auto& other : agents_) {
      if (other.id == ag->id) continue;
      const double d = std::hypot(other.pos.x - ag->pos.x, other.pos.y - ag->pos.y);
      if (d <= kHearingRadius) emit(other, "heard", {{"from", ag->id}, {"to", to}, {"text", text}});
    }
    return accept("");
  }

  if (out.type == "check_market") {
    json history = json::object();
    for (const auto& [item, h] : price_history_) history[item] = std::vector<double>(h.begin(), h.end());
    json prices = json::object();
    for (const auto& [item, p] : prices_) prices[item] = round2(p);
    emit(*ag, "market_report", {{"prices", prices}, {"history", history}});
    return accept("");
  }

  return reject("unknown_action");
}

// ---------------------------------------------------------------- tick

void World::update_market() {
  std::normal_distribution<double> noise(0.0, 0.06);
  const std::map<std::string, double> base{{"ore", 5.0}, {"tool", 20.0}};
  for (auto& [item, price] : prices_) {
    // Random walk that slowly drifts back toward the base price.
    const double drift = (base.at(item) - price) * 0.05;
    price = std::clamp(price * (1.0 + noise(rng_)) + drift, 1.0, base.at(item) * 3.0);
    auto& h = price_history_[item];
    h.push_back(round2(price));
    while (h.size() > kMarketHistory) h.pop_front();
  }
}

void World::step() {
  ++tick_;

  for (auto& a : agents_) {
    if (a.bubble_ttl > 0 && --a.bubble_ttl == 0) a.bubble.clear();
    if (a.path.empty()) continue;
    if (a.move_cooldown > 0) {
      --a.move_cooldown;
      continue;
    }

    const Point next = a.path.front();
    if (occupied_by_other(next, a)) {
      // Close enough to the destination? Count it as arrived rather than queueing forever.
      if (!a.destination.empty() && chebyshev(a.pos, landmarks_[a.destination]) <= kAtLocationRadius) {
        a.path.clear();
        const std::string dest = a.destination;
        a.destination.clear();
        emit(a, "arrived", {{"location", dest}});
        continue;
      }
      // Wait a moment, then re-plan around whoever is in the way.
      if (++a.blocked_ticks >= kBlockedRepathTicks) {
        auto others = other_agent_tiles(a);
        auto route = astar(grid_, a.pos, a.goal, &others);
        a.blocked_ticks = 0;
        if (route.empty()) {
          a.path.clear();
          const std::string dest = a.destination;
          a.destination.clear();
          emit(a, "action_rejected", {{"action", "move_to"}, {"reason", "blocked"}, {"target", dest}});
        } else {
          a.path.assign(route.begin(), route.end());
        }
      }
      continue;
    }

    a.pos = next;
    a.path.pop_front();
    a.blocked_ticks = 0;
    a.move_cooldown = kTicksPerStep - 1;
    if (a.path.empty()) {
      const std::string dest = a.destination;
      a.destination.clear();
      emit(a, "arrived", {{"location", dest}});
    }
  }

  if (tick_ % kMineRegenTicks == 0 && mine_stock_ < kMineMaxStock) ++mine_stock_;
  if (tick_ % kMarketUpdateTicks == 0) update_market();
}

std::string World::snapshot_json() const {
  json agents = json::array();
  for (const auto& a : agents_) {
    agents.push_back({{"id", a.id},
                      {"role", a.role},
                      {"x", a.pos.x},
                      {"y", a.pos.y},
                      {"location", location_of(a)},
                      {"destination", a.destination},
                      {"inventory", a.inventory},
                      {"coins", a.coins},
                      {"bubble", a.bubble},
                      {"moving", !a.path.empty()}});
  }
  json landmarks = json::object();
  for (const auto& [name, p] : landmarks_) landmarks[name] = {p.x, p.y};
  json prices = json::object();
  for (const auto& [item, p] : prices_) prices[item] = round2(p);

  return json{{"type", "snapshot"},
              {"tick", tick_},
              {"width", grid_.width},
              {"height", grid_.height},
              {"tiles", grid_.rows},
              {"landmarks", landmarks},
              {"mine_stock", mine_stock_},
              {"prices", prices},
              {"agents", agents}}
      .dump();
}
