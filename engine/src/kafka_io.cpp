#include "kafka_io.hpp"

#include <iostream>

bool KafkaIO::start(std::string& err) {
  std::unique_ptr<RdKafka::Conf> pconf(RdKafka::Conf::create(RdKafka::Conf::CONF_GLOBAL));
  if (pconf->set("bootstrap.servers", brokers_, err) != RdKafka::Conf::CONF_OK) return false;
  pconf->set("linger.ms", "5", err);          // batch tiny messages for up to 5 ms
  pconf->set("client.id", "worldcraft-engine", err);
  producer_.reset(RdKafka::Producer::create(pconf.get(), err));
  if (!producer_) return false;

  std::unique_ptr<RdKafka::Conf> cconf(RdKafka::Conf::create(RdKafka::Conf::CONF_GLOBAL));
  if (cconf->set("bootstrap.servers", brokers_, err) != RdKafka::Conf::CONF_OK) return false;
  cconf->set("group.id", "engine", err);
  // Only act on actions sent while the engine is running; never replay yesterday's.
  cconf->set("auto.offset.reset", "latest", err);
  cconf->set("enable.partition.eof", "false", err);
  consumer_.reset(RdKafka::KafkaConsumer::create(cconf.get(), err));
  if (!consumer_) return false;

  RdKafka::ErrorCode rc = consumer_->subscribe({"agent.actions"});
  if (rc != RdKafka::ERR_NO_ERROR) {
    err = RdKafka::err2str(rc);
    return false;
  }

  running_ = true;
  thread_ = std::thread(&KafkaIO::consume_loop, this);
  return true;
}

void KafkaIO::consume_loop() {
  while (running_) {
    std::unique_ptr<RdKafka::Message> msg(consumer_->consume(100));
    if (msg->err() == RdKafka::ERR__TIMED_OUT) continue;
    if (msg->err() != RdKafka::ERR_NO_ERROR) {
      std::cerr << "[kafka] consume error: " << msg->errstr() << "\n";
      continue;
    }
    const std::string payload(static_cast<const char*>(msg->payload()), msg->len());
    auto parsed = nlohmann::json::parse(payload, nullptr, /*allow_exceptions=*/false);
    if (parsed.is_discarded() || !parsed.is_object()) {
      ++parse_errors_;
      continue;
    }
    std::lock_guard<std::mutex> lock(mu_);
    queue_.push_back(std::move(parsed));
  }
}

std::vector<nlohmann::json> KafkaIO::drain_actions() {
  std::vector<nlohmann::json> out;
  std::lock_guard<std::mutex> lock(mu_);
  out.swap(queue_);
  return out;
}

void KafkaIO::produce(const std::string& topic, const std::string& key, const std::string& payload) {
  RdKafka::ErrorCode rc = producer_->produce(
      topic, RdKafka::Topic::PARTITION_UA, RdKafka::Producer::RK_MSG_COPY,
      const_cast<char*>(payload.data()), payload.size(), key.data(), key.size(), 0, nullptr);
  if (rc == RdKafka::ERR__QUEUE_FULL) {
    producer_->poll(10);  // let delivery reports drain, then drop this message
    ++produce_errors_;
  } else if (rc != RdKafka::ERR_NO_ERROR) {
    ++produce_errors_;
  }
}

void KafkaIO::stop() {
  if (!running_.exchange(false)) return;
  if (thread_.joinable()) thread_.join();
  if (consumer_) consumer_->close();
  if (producer_) producer_->flush(2000);
}
