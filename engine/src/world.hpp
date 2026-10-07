#pragma once
#include <deque>
#include <map>
#include <nlohmann/json.hpp>
#include <random>
#include <string>
#include <vector>

#include "pathfinding.hpp"

using json = nlohmann::json;

// Tunables. At 20 ticks/second a cook moving every 3 ticks walks ~7 tiles/second
// (a kitchen is small and people move fast).
constexpr int kTicksPerSecond = 20;
constexpr int kTicksPerStep = 3;
constexpr int kBlockedRepathTicks = 10;
constexpr int kBubbleTicks = 5 * kTicksPerSecond;
constexpr int kHearingRadius = 12;
constexpr int kAtStationRadius = 2;

constexpr int kStockMax = 10;                            // portions of prep in the walk-in
constexpr int kRestockTicks = 8 * kTicksPerSecond;       // a delivery arrives this often
// Service runs on its own clock: one real second is one restaurant minute,
// starting at 11:00. Ticket pace depends on the hour, like a real service.
constexpr int kServiceStartMinutes = 11 * 60;
constexpr int kTicketDueTicks = 90 * kTicksPerSecond;    // a table waits this long
constexpr int kTicketGraceTicks = 30 * kTicksPerSecond;  // then walks out
constexpr size_t kMaxOpenTickets = 8;
constexpr size_t kMenuMax = 9;

struct Character {
  std::string id;
  std::string name;
  std::string role;
  std::string color;
  char spawn = '1';
};

struct Cook {
  std::string id;
  std::string name;
  std::string role;
  std::string color;
  Point pos;
  std::deque<Point> path;
  Point goal;
  std::string destination;  // station name while walking, "" when standing still
  int move_cooldown = 0;
  int blocked_ticks = 0;
  std::map<std::string, int> inventory{{"prep", 0}};  // "prep" plus finished dishes by name
  int tips = 0;
  std::string bubble;
  int bubble_ttl = 0;
};

struct Ticket {
  int id = 0;
  std::string dish;
  long placed_tick = 0;
  long due_tick = 0;
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
  bool load_cast(const std::string& path, std::string& err);
  void spawn_cast();

  ActionOutcome apply_action(const json& action);
  void step();

  std::string snapshot_json() const;
  std::vector<OutMessage> drain_outbox();

  long tick() const { return tick_; }
  int stock() const { return stock_; }
  int open_tickets() const { return static_cast<int>(tickets_.size()); }
  long served() const { return served_; }
  long walkouts() const { return walkouts_; }
  size_t cook_count() const { return cooks_.size(); }

 private:
  Cook* find_cook(const std::string& id);
  std::string station_of(const Cook& c) const;
  Cook* cook_at(Point p, const Cook& self);
  bool occupied_by_other(Point p, const Cook& self) const;
  std::unordered_set<int> other_cook_tiles(const Cook& self) const;
  json state_of(const Cook& c) const;
  json tickets_json() const;
  void emit(const Cook& c, const std::string& type, json extra = json::object());
  void emit_to_role(const std::string& role, const std::string& type, json extra);
  void new_ticket();
  int clock_minutes() const { return kServiceStartMinutes + static_cast<int>(tick_ / kTicksPerSecond); }
  std::string clock_string() const;
  std::string phase() const;
  void ticket_gap(int& min_ticks, int& max_ticks) const;
  void expire_tickets();
  bool on_menu(const std::string& dish) const;

  Grid grid_;
  std::map<std::string, Point> stations_;
  std::map<char, Point> spawns_;
  std::vector<Character> cast_;
  std::vector<Cook> cooks_;
  std::vector<OutMessage> outbox_;
  std::vector<Ticket> tickets_;
  std::vector<std::string> menu_{"beef sandwich", "chopped salad", "fries", "chocolate cake"};
  std::string restaurant_ = "The Original Beef";
  long tick_ = 0;
  long next_ticket_tick_ = 4 * kTicksPerSecond;
  int next_ticket_id_ = 1;
  int stock_ = kStockMax;
  long served_ = 0;
  long walkouts_ = 0;
  std::mt19937 rng_{42};
};
