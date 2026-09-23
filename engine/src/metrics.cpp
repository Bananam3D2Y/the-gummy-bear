#include "metrics.hpp"

#include <httplib.h>

#include <sstream>

void Histogram::observe(double v) {
  for (size_t i = 0; i < bounds_.size(); ++i)
    if (v <= bounds_[i]) ++counts_[i];
  ++count_;
  sum_ += v;
}

std::string Histogram::render(const std::string& name, const std::string& help) const {
  std::ostringstream os;
  os << "# HELP " << name << " " << help << "\n# TYPE " << name << " histogram\n";
  for (size_t i = 0; i < bounds_.size(); ++i)
    os << name << "_bucket{le=\"" << bounds_[i] << "\"} " << counts_[i] << "\n";
  os << name << "_bucket{le=\"+Inf\"} " << count_ << "\n";
  os << name << "_sum " << sum_ << "\n" << name << "_count " << count_ << "\n";
  return os.str();
}

Metrics::Metrics()
    : tick_duration_({0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05}),
      staleness_({1, 5, 10, 20, 40, 80, 160, 320, 640}) {}

Metrics::~Metrics() { stop(); }

void Metrics::start(int port) {
  server_ = std::make_unique<httplib::Server>();
  server_->Get("/metrics", [this](const httplib::Request&, httplib::Response& res) {
    res.set_content(render(), "text/plain; version=0.0.4");
  });
  server_->Get("/health", [](const httplib::Request&, httplib::Response& res) {
    res.set_content("ok", "text/plain");
  });
  thread_ = std::thread([this, port] { server_->listen("0.0.0.0", port); });
}

void Metrics::stop() {
  if (server_) server_->stop();
  if (thread_.joinable()) thread_.join();
}

void Metrics::observe_tick(double seconds) {
  std::lock_guard<std::mutex> lock(mu_);
  tick_duration_.observe(seconds);
  ++ticks_;
}

void Metrics::record_action(const ActionOutcome& o) {
  std::lock_guard<std::mutex> lock(mu_);
  ++actions_[{o.type, o.accepted ? "accepted" : "rejected", o.reason}];
  staleness_.observe(static_cast<double>(o.staleness_ticks));
}

void Metrics::set_gauges(size_t agents, int mine_stock, long tick, long parse_errors, long produce_errors) {
  std::lock_guard<std::mutex> lock(mu_);
  agents_ = agents;
  mine_stock_ = mine_stock;
  tick_ = tick;
  parse_errors_ = parse_errors;
  produce_errors_ = produce_errors;
}

std::string Metrics::render() {
  std::lock_guard<std::mutex> lock(mu_);
  std::ostringstream os;
  os << "# HELP engine_ticks_total Simulation ticks executed.\n# TYPE engine_ticks_total counter\n"
     << "engine_ticks_total " << ticks_ << "\n";
  os << tick_duration_.render("engine_tick_duration_seconds", "Wall time spent inside one tick.");
  os << staleness_.render("engine_action_staleness_ticks",
                          "Ticks between the world state an agent decided on and when its action arrived.");
  os << "# HELP engine_actions_total Actions received, by type and outcome.\n# TYPE engine_actions_total counter\n";
  for (const auto& [k, v] : actions_) {
    os << "engine_actions_total{type=\"" << std::get<0>(k) << "\",result=\"" << std::get<1>(k)
       << "\",reason=\"" << std::get<2>(k) << "\"} " << v << "\n";
  }
  os << "# TYPE engine_agents gauge\nengine_agents " << agents_ << "\n";
  os << "# TYPE engine_mine_stock gauge\nengine_mine_stock " << mine_stock_ << "\n";
  os << "# TYPE engine_current_tick gauge\nengine_current_tick " << tick_ << "\n";
  os << "# TYPE engine_kafka_parse_errors_total counter\nengine_kafka_parse_errors_total " << parse_errors_ << "\n";
  os << "# TYPE engine_kafka_produce_errors_total counter\nengine_kafka_produce_errors_total " << produce_errors_ << "\n";
  return os.str();
}
