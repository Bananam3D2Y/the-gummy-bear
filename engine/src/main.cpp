#include <chrono>
#include <csignal>
#include <cstdlib>
#include <iostream>
#include <thread>

#include "kafka_io.hpp"
#include "metrics.hpp"
#include "world.hpp"

namespace {
std::atomic<bool> g_running{true};
void on_signal(int) { g_running = false; }

std::string env_or(const char* name, const std::string& fallback) {
  const char* v = std::getenv(name);
  return v && *v ? std::string(v) : fallback;
}
}  // namespace

int main() {
  const std::string brokers = env_or("KAFKA_BROKERS", "localhost:9092");
  const std::string map_path = env_or("MAP_PATH", "map.txt");
  const std::string cast_path = env_or("CAST_PATH", "../cast.json");
  const int metrics_port = std::stoi(env_or("METRICS_PORT", "9100"));

  std::signal(SIGINT, on_signal);
  std::signal(SIGTERM, on_signal);

  World world;
  std::string err;
  if (!world.load_map(map_path, err)) {
    std::cerr << "[engine] " << err << "\n";
    return 1;
  }
  if (!world.load_cast(cast_path, err)) {
    std::cerr << "[engine] " << err << "\n";
    return 1;
  }
  world.spawn_cast();

  Metrics metrics;
  metrics.start(metrics_port);

  KafkaIO kafka(brokers);
  if (!kafka.start(err)) {
    std::cerr << "[engine] kafka start failed: " << err << "\n";
    return 1;
  }
  std::cout << "[engine] running: brokers=" << brokers << " metrics=:" << metrics_port
            << " cooks=" << world.cook_count() << "\n";

  // Fixed timestep: every tick is 50 ms of simulated time, no matter how long the work took.
  using clock = std::chrono::steady_clock;
  const auto tick_len = std::chrono::milliseconds(1000 / kTicksPerSecond);
  auto next = clock::now();

  while (g_running) {
    const auto t0 = clock::now();

    // 1. Apply every action that arrived since the last tick.
    for (const auto& action : kafka.drain_actions()) {
      const auto outcome = world.apply_action(action);
      metrics.record_action(outcome);
      std::cout << "[engine] t=" << world.tick() << " " << action.value("agent_id", "?") << " "
                << outcome.type << " -> " << (outcome.accepted ? "ok" : "REJECTED " + outcome.reason)
                << " (stale " << outcome.staleness_ticks << " ticks)\n";
    }

    // 2. Advance the simulation one step.
    world.step();

    // 3. Publish perception events and, at 10 Hz, a full snapshot for the browser.
    for (const auto& m : world.drain_outbox()) kafka.produce(m.topic, m.key, m.payload);
    if (world.tick() % 2 == 0) kafka.produce("world.snapshots", "world", world.snapshot_json());
    kafka.poll();

    const double took = std::chrono::duration<double>(clock::now() - t0).count();
    metrics.observe_tick(took);
    metrics.set_gauges(world.cook_count(), world.stock(), world.open_tickets(), world.served(),
                       world.walkouts(), world.tick(), kafka.parse_errors(), kafka.produce_errors());

    next += tick_len;
    const auto now = clock::now();
    if (now > next + std::chrono::seconds(1)) next = now;  // fell far behind (debugger, laptop sleep): resync
    std::this_thread::sleep_until(next);
  }

  std::cout << "[engine] shutting down\n";
  kafka.stop();
  metrics.stop();
  return 0;
}
