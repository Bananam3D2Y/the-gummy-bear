#pragma once
#include <deque>
#include <map>
#include <nlohmann/json.hpp>
#include <random>
#include <string>
#include <vector>

#include "pathfinding.hpp"

using json = nlohmann::json;

// Tunables. At 20 ticks/second an agent moving every 4 ticks walks 5 tiles/second.
constexpr int kTicksPerSecond = 20;
constexpr int kTicksPerStep = 4;
constexpr int kBlockedRepathTicks = 10;
constexpr int kBubbleTicks = 5 * kTicksPerSecond;
constexpr int kHearingRadius = 10;
constexpr int kAtLocationRadius = 2;
constexpr int kMineMaxStock = 6;
constexpr int kMineRegenTicks = 10 * kTicksPerSecond;
constexpr int kMarketUpdateTicks = 5 * kTicksPerSecond;
constexpr size_t kMarketHistory = 20;

struct Agent {
  std::string id;
  std::string role;
  Point pos;
  std::deque<Point> path;
  Point goal;
  std::string destination;  // landmark name while walking, "" when idle
  int move_cooldown = 0;
  int blocked_ticks = 0;
  std::map<std::string, int> inventory{{"ore", 0}, {"tool", 0}};
  int coins = 0;
  std::string bubble;  // speech bubble text shown in the browser
  int bubble_ttl = 0;
};

// A message the tick loop should publish to Kafka after the tick.
struct OutMessage {
  std::string topic;
  std::string key;
  std::string payload;
};

// What happened to an action; feeds the Prometheus counters.
struct ActionOutcome {
  std::string type;
  bool accepted = false;
  std::string reason;
  long staleness_ticks = 0;
};

class World {
 public:
  bool load_map(const std::string& path, std::string& err);
  void spawn_agents();

  ActionOutcome apply_action(const json& action);
  void step();

  std::string snapshot_json() const;
  std::vector<OutMessage> drain_outbox();

  long tick() const { return tick_; }
  int mine_stock() const { return mine_stock_; }
  size_t agent_count() const { return agents_.size(); }

 private:
  Agent* find_agent(const std::string& id);
  std::string location_of(const Agent& a) const;
  bool occupied_by_other(Point p, const Agent& self) const;
  std::unordered_set<int> other_agent_tiles(const Agent& self) const;
  json state_of(const Agent& a) const;
  void emit(const Agent& a, const std::string& type, json extra = json::object());
  void update_market();

  Grid grid_;
  std::map<std::string, Point> landmarks_;
  std::map<char, Point> spawns_;
  std::vector<Agent> agents_;
  std::vector<OutMessage> outbox_;
  long tick_ = 0;
  int mine_stock_ = kMineMaxStock;
  std::map<std::string, double> prices_{{"ore", 5.0}, {"tool", 20.0}};
  std::map<std::string, std::deque<double>> price_history_;
  std::mt19937 rng_{42};
};
