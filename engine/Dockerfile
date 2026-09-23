# Build stage: compile the engine with all headers fetched by CMake
FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake git pkg-config librdkafka-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY CMakeLists.txt ./
COPY src ./src
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j

# Runtime stage: just the binary, the map, and the Kafka runtime library
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends librdkafka++1 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /src/build/engine /app/engine
COPY map.txt /app/map.txt
EXPOSE 9100
CMD ["./engine"]
