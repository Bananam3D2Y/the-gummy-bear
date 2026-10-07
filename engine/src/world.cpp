#include "world.hpp"

#include <algorithm>
#include <cmath>
#include <cctype>
#include <cstdio>
#include <fstream>

namespace {
const char* kEventsTopic = "world.events";
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
  // Letters mark stations, digits mark spawn points.
  for (int y = 0; y < grid_.height; ++y) {
    for (int x = 0; x < grid_.width; ++x) {
      const char c = grid_.at(x, y);
      switch (c) {
        case 'W': stations_["walkin"] = {x, y}; break;
        case 'L': stations_["line"] = {x, y}; break;
        case 'P': stations_["pass"] = {x, y}; break;
        case 'A': stations_["alley"] = {x, y}; break;
        default:
          if (c >= '1' && c <= '9') spawns_[c] = {x, y};
          break;
      }
    }
  }
  for (const char* required : {"walkin", "line", "pass", "alley"}) {
    if (!stations_.count(required)) {
      err = std::string("map is missing station: ") + required;
      return false;
    }
  }
  return true;
}

bool World::load_cast(const std::string& path, std::string& err) {
  std::ifstream in(path);
  if (!in) {
    err = "cannot open cast file: " + path;
    return false;
  }
  json j = json::parse(in, nullptr, false);
  if (j.is_discarded() || !j.contains("characters")) {
    err = "cast file is not valid JSON with a \"characters\" array: " + path;
    return false;
  }
  restaurant_ = j.value("restaurant", restaurant_);
  for (const auto& c : j["characters"]) {
    Character ch;
    ch.id = c.value("id", "");
    ch.name = c.value("name", ch.id);
    ch.role = c.value("role", "cook");
    ch.color = c.value("color", "#dddddd");
    const std::string spawn = c.value("spawn", "1");
    ch.spawn = spawn.empty() ? '1' : spawn[0];
    if (ch.id.empty()) {
      err = "a character in the cast file has no id";
      return false;
    }
    cast_.push_back(ch);
  }
  if (cast_.empty()) {
    err = "cast file has no characters";
    return false;
  }
  return true;
}

void World::spawn_cast() {
  for (const auto& ch : cast_) {
    Cook c;
    c.id = ch.id;
    c.name = ch.name;
    c.role = ch.role;
    c.color = ch.color;
    c.pos = spawns_.count(ch.spawn) ? spawns_[ch.spawn] : stations_["line"];
    cooks_.push_back(c);
  }
}

// ---------------------------------------------------------------- helpers

Cook* World::find_cook(const std::string& id) {
  for (auto& c : cooks_)
    if (c.id == id) return &c;
  return nullptr;
}

std::string World::station_of(const Cook& c) const {
  for (const auto& [name, p] : stations_)
    if (chebyshev(c.pos, p) <= kAtStationRadius) return name;
  return "";
}

Cook* World::cook_at(Point p, const Cook& self) {
  for (auto& o : cooks_)
    if (o.id != self.id && o.pos == p) return &o;
  return nullptr;
}

bool World::occupied_by_other(Point p, const Cook& self) const {
  for (const auto& o : cooks_)
    if (o.id != self.id && o.pos == p) return true;
  return false;
}

std::unordered_set<int> World::other_cook_tiles(const Cook& self) const {
  std::unordered_set<int> s;
  for (const auto& o : cooks_)
    if (o.id != self.id) s.insert(grid_.index(o.pos.x, o.pos.y));
  return s;
}

bool World::on_menu(const std::string& dish) const {
  return std::find(menu_.begin(), menu_.end(), dish) != menu_.end();
}

json World::state_of(const Cook& c) const {
  return {{"x", c.pos.x},
          {"y", c.pos.y},
          {"location", station_of(c)},
          {"inventory", c.inventory},
          {"coins", c.tips},
          {"moving", !c.path.empty()},
          {"destination", c.destination}};
}

json World::tickets_json() const {
  json arr = json::array();
  for (const auto& t : tickets_) {
    arr.push_back({{"id", t.id},
                   {"dish", t.dish},
                   {"waiting_s", (tick_ - t.placed_tick) / kTicksPerSecond},
                   {"due_in_s", (t.due_tick - tick_) / kTicksPerSecond}});
  }
  return arr;
}

// Every event carries the cook's current state so the brain never has to ask for it.
void World::emit(const Cook& c, const std::string& type, json extra) {
  extra["type"] = type;
  extra["agent_id"] = c.id;
  extra["tick"] = tick_;
  extra["state"] = state_of(c);
  outbox_.push_back({kEventsTopic, c.id, extra.dump()});
}

// Ticket news goes to whoever runs the rail, not to all seven cooks: one
// expediter thinking is one LLM call, seven is seven.
void World::emit_to_role(const std::string& role, const std::string& type, json extra) {
  for (const auto& c : cooks_)
    if (c.role == role) emit(c, type, extra);
}

std::vector<OutMessage> World::drain_outbox() {
  std::vector<OutMessage> out;
  out.swap(outbox_);
  return out;
}

// ---------------------------------------------------------------- actions

// The engine is the referee: brains only REQUEST actions, and every request is
// validated against the kitchen as it is NOW, not as it was when the LLM decided.
ActionOutcome World::apply_action(const json& action) {
  ActionOutcome out;
  out.type = action.value("type", std::string("unknown"));
  const std::string agent_id = action.value("agent_id", std::string());
  const long based_on = action.value("based_on_tick", tick_);
  out.staleness_ticks = std::max(0L, tick_ - based_on);

  Cook* ag = find_cook(agent_id);
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

  const std::string here = station_of(*ag);

  if (out.type == "move_to") {
    const std::string target = action.value("target", std::string());
    auto it = stations_.find(target);
    if (it == stations_.end()) return reject("unknown_station");
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

  if (out.type == "pull_stock") {
    if (here != "walkin") return reject("not_at_walkin");
    if (stock_ <= 0) return reject("out_of_stock");
    --stock_;
    ++ag->inventory["prep"];
    return accept("pulled 1 portion of prep (walk-in has " + std::to_string(stock_) + " left)");
  }

  if (out.type == "cook") {
    const std::string dish = action.value("dish", std::string());
    if (!on_menu(dish)) return reject("not_on_menu");
    if (here != "line") return reject("not_at_line");
    if (ag->inventory["prep"] < 2) return reject("not_enough_prep");
    ag->inventory["prep"] -= 2;
    ++ag->inventory[dish];
    return accept("fired 1 " + dish + " (2 prep)");
  }

  if (out.type == "serve") {
    const std::string dish = action.value("dish", std::string());
    if (here != "pass") return reject("not_at_pass");
    if (ag->inventory[dish] < 1) return reject("dish_missing");
    // Serve the ticket that has been waiting longest for this dish.
    auto it = std::min_element(tickets_.begin(), tickets_.end(),
                               [&](const Ticket& a, const Ticket& b) {
                                 if (a.dish != dish) return false;
                                 if (b.dish != dish) return true;
                                 return a.placed_tick < b.placed_tick;
                               });
    if (it == tickets_.end() || it->dish != dish) return reject("no_ticket_for_dish");
    const bool on_time = tick_ <= it->due_tick;
    const int tip = on_time ? 10 : 3;
    --ag->inventory[dish];
    ag->tips += tip;
    ++served_;
    const int waited = static_cast<int>((tick_ - it->placed_tick) / kTicksPerSecond);
    tickets_.erase(it);
    return accept("served ticket for " + dish + " after " + std::to_string(waited) + "s (" +
                  (on_time ? "on time" : "LATE") + ", " + std::to_string(tip) + " tip)");
  }

  if (out.type == "hand") {
    const std::string item = action.value("item", std::string());
    const std::string to = action.value("to", std::string());
    Cook* rec = find_cook(to);
    if (!rec || rec == ag) return reject("unknown_agent");
    if (ag->inventory[item] < 1) return reject("item_missing");
    if (chebyshev(ag->pos, rec->pos) > kAtStationRadius) return reject("target_not_nearby");
    --ag->inventory[item];
    ++rec->inventory[item];
    emit(*rec, "received", {{"from", ag->id}, {"item", item}});
    return accept("handed 1 " + item + " to " + rec->name);
  }

  if (out.type == "say") {
    std::string text = action.value("text", std::string());
    if (text.empty()) return reject("empty_text");
    if (text.size() > 200) text.resize(200);
    const std::string to = action.value("to", std::string());
    ag->bubble = text;
    ag->bubble_ttl = kBubbleTicks;
    // A kitchen is small: everyone within earshot hears it.
    for (auto& other : cooks_) {
      if (other.id == ag->id) continue;
      const double d = std::hypot(other.pos.x - ag->pos.x, other.pos.y - ag->pos.y);
      if (d <= kHearingRadius) emit(other, "heard", {{"from", ag->id}, {"to", to}, {"text", text}});
    }
    return accept("");
  }

  if (out.type == "bin") {
    const std::string item = action.value("item", std::string());
    if (ag->inventory[item] < 1) return reject("item_missing");
    --ag->inventory[item];
    return accept("scraped 1 " + item + " into the bin");
  }

  if (out.type == "create_dish") {
    std::string name = action.value("dish", std::string());
    if (name.empty() || name.size() > 40) return reject("bad_dish_name");
    for (auto& ch : name) ch = static_cast<char>(std::tolower(static_cast<unsigned char>(ch)));
    if (on_menu(name)) return reject("already_on_menu");
    if (menu_.size() >= kMenuMax) return reject("menu_full");
    menu_.push_back(name);
    // Everyone needs to know the menu changed: they can be asked to cook it.
    for (auto& other : cooks_)
      if (other.id != ag->id) emit(other, "menu_changed", {{"dish", name}, {"by", ag->name}});
    return accept("put \"" + name + "\" on the menu");
  }

  if (out.type == "check_tickets") {
    emit(*ag, "ticket_report", {{"tickets", tickets_json()},
                                {"clock", clock_string()},
                                {"phase", phase()},
                                {"clock", clock_string()},
              {"phase", phase()},
              {"stock", stock_},
                                {"menu", menu_},
                                {"served", served_},
                                {"walkouts", walkouts_}});
    return accept("");
  }

  return reject("unknown_action");
}

// ---------------------------------------------------------------- tick

std::string World::clock_string() const {
  const int m = clock_minutes() % (24 * 60);
  char buf[8];
  std::snprintf(buf, sizeof(buf), "%02d:%02d", m / 60, m % 60);
  return std::string(buf);
}

// The shape of a service day: quiet open, lunch rush, afternoon lull, dinner rush, close.
std::string World::phase() const {
  const int h = (clock_minutes() % (24 * 60)) / 60;
  if (h < 12) return "opening";
  if (h < 14) return "lunch rush";
  if (h < 17) return "afternoon lull";
  if (h < 21) return "dinner rush";
  return "closing";
}

void World::ticket_gap(int& min_ticks, int& max_ticks) const {
  const std::string p = phase();
  int lo = 30, hi = 55;
  if (p == "lunch rush") { lo = 10; hi = 20; }
  else if (p == "dinner rush") { lo = 8; hi = 18; }
  else if (p == "afternoon lull") { lo = 45; hi = 80; }
  else if (p == "closing") { lo = 60; hi = 110; }
  min_ticks = lo * kTicksPerSecond;
  max_ticks = hi * kTicksPerSecond;
}

void World::new_ticket() {
  if (tickets_.size() >= kMaxOpenTickets) return;
  std::uniform_int_distribution<size_t> pick(0, menu_.size() - 1);
  Ticket t;
  t.id = next_ticket_id_++;
  t.dish = menu_[pick(rng_)];
  t.placed_tick = tick_;
  t.due_tick = tick_ + kTicketDueTicks;
  tickets_.push_back(t);
  emit_to_role("expediter", "ticket_in",
               {{"ticket_id", t.id}, {"dish", t.dish}, {"open_tickets", tickets_.size()}});
}

void World::expire_tickets() {
  for (auto it = tickets_.begin(); it != tickets_.end();) {
    if (tick_ > it->due_tick + kTicketGraceTicks) {
      ++walkouts_;
      emit_to_role("expediter", "walkout", {{"ticket_id", it->id}, {"dish", it->dish},
                                            {"walkouts", walkouts_}});
      it = tickets_.erase(it);
    } else {
      ++it;
    }
  }
}

void World::step() {
  ++tick_;

  for (auto& a : cooks_) {
    if (a.bubble_ttl > 0 && --a.bubble_ttl == 0) a.bubble.clear();
    if (a.path.empty()) continue;
    if (a.move_cooldown > 0) {
      --a.move_cooldown;
      continue;
    }

    const Point next = a.path.front();
    if (Cook* other = cook_at(next, a)) {
      // Head-on in a narrow gangway: they step around each other instead of
      // both waiting, which used to deadlock the pair until someone gave up.
      if (!other->path.empty() && other->path.front() == a.pos && other->move_cooldown == 0) {
        std::swap(a.pos, other->pos);
        a.path.pop_front();
        other->path.pop_front();
        a.blocked_ticks = other->blocked_ticks = 0;
        a.move_cooldown = other->move_cooldown = kTicksPerStep - 1;
        for (Cook* c : {&a, other}) {
          if (c->path.empty() && !c->destination.empty()) {
            const std::string dest = c->destination;
            c->destination.clear();
            emit(*c, "arrived", {{"location", dest}});
          }
        }
        continue;
      }
      // Close enough to the station? Count it as arrived rather than queueing forever.
      if (!a.destination.empty() && chebyshev(a.pos, stations_[a.destination]) <= kAtStationRadius) {
        a.path.clear();
        const std::string dest = a.destination;
        a.destination.clear();
        emit(a, "arrived", {{"location", dest}});
        continue;
      }
      // Wait a moment, then squeeze past whoever is in the way.
      if (++a.blocked_ticks >= kBlockedRepathTicks) {
        auto others = other_cook_tiles(a);
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

  if (tick_ % kRestockTicks == 0 && stock_ < kStockMax) ++stock_;
  if (tick_ >= next_ticket_tick_) {
    new_ticket();
    int lo = 0, hi = 0;
    ticket_gap(lo, hi);
    std::uniform_int_distribution<int> gap(lo, hi);
    next_ticket_tick_ = tick_ + gap(rng_);
  }
  expire_tickets();
}

std::string World::snapshot_json() const {
  json agents = json::array();
  for (const auto& a : cooks_) {
    agents.push_back({{"id", a.id},
                      {"name", a.name},
                      {"role", a.role},
                      {"color", a.color},
                      {"x", a.pos.x},
                      {"y", a.pos.y},
                      {"location", station_of(a)},
                      {"destination", a.destination},
                      {"inventory", a.inventory},
                      {"coins", a.tips},
                      {"bubble", a.bubble},
                      {"moving", !a.path.empty()}});
  }
  json stations = json::object();
  for (const auto& [name, p] : stations_) stations[name] = {p.x, p.y};

  return json{{"type", "snapshot"},
              {"tick", tick_},
              {"restaurant", restaurant_},
              {"width", grid_.width},
              {"height", grid_.height},
              {"tiles", grid_.rows},
              {"landmarks", stations},
              {"clock", clock_string()},
              {"phase", phase()},
              {"stock", stock_},
              {"tickets", tickets_json()},
              {"menu", menu_},
              {"served", served_},
              {"walkouts", walkouts_},
              {"agents", agents}}
      .dump();
}
