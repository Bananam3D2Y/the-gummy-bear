#pragma once
#include <librdkafka/rdkafkacpp.h>

#include <atomic>
#include <memory>
#include <mutex>
#include <nlohmann/json.hpp>
#include <string>
#include <thread>
#include <vector>

// Owns the Kafka producer (used by the tick thread) and a consumer running on
// its own thread. Kafka I/O never blocks the tick loop: the consumer thread
// pushes parsed actions into a mutex-guarded queue that the tick loop drains.
class KafkaIO {
 public:
  explicit KafkaIO(std::string brokers) : brokers_(std::move(brokers)) {}
  ~KafkaIO() { stop(); }

  bool start(std::string& err);
  void stop();

  void produce(const std::string& topic, const std::string& key, const std::string& payload);
  void poll() { if (producer_) producer_->poll(0); }

  std::vector<nlohmann::json> drain_actions();
  long parse_errors() const { return parse_errors_.load(); }
  long produce_errors() const { return produce_errors_.load(); }

 private:
  void consume_loop();

  std::string brokers_;
  std::unique_ptr<RdKafka::Producer> producer_;
  std::unique_ptr<RdKafka::KafkaConsumer> consumer_;
  std::thread thread_;
  std::atomic<bool> running_{false};
  std::mutex mu_;
  std::vector<nlohmann::json> queue_;
  std::atomic<long> parse_errors_{0};
  std::atomic<long> produce_errors_{0};
};
