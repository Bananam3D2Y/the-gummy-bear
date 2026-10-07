#pragma once
#include <atomic>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <tuple>
#include <vector>

#include "world.hpp"

namespace httplib { class Server; }

// Minimal Prometheus histogram (cumulative buckets, sum, count).
class Histogram {
 public:
  explicit Histogram(std::vector<double> bounds) : bounds_(std::move(bounds)), counts_(bounds_.size(), 0) {}
  void observe(double v);
  std::string render(const std::string& name, const std::string& help) const;

 private:
  std::vector<double> bounds_;
  std::vector<long> counts_;
  long count_ = 0;
  double sum_ = 0.0;
};

// Collects engine metrics and serves them at http://0.0.0.0:<port>/metrics
class Metrics {
 public:
  Metrics();
  ~Metrics();
  void start(int port);
  void stop();

  void observe_tick(double seconds);
  void record_action(const ActionOutcome& o);
  void set_gauges(size_t cooks, int stock, int open_tickets, long served, long walkouts,
                  long tick, long parse_errors, long produce_errors);
  std::string render();

 private:
  std::mutex mu_;
  Histogram tick_duration_;
  Histogram staleness_;
  long ticks_ = 0;
  std::map<std::tuple<std::string, std::string, std::string>, long> actions_;
  size_t cooks_ = 0;
  int stock_ = 0;
  int open_tickets_ = 0;
  long served_ = 0;
  long walkouts_ = 0;
  long tick_ = 0;
  long parse_errors_ = 0;
  long produce_errors_ = 0;
  std::unique_ptr<httplib::Server> server_;
  std::thread thread_;
};
