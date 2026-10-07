// Run: ./build/pathfinding_test map.txt
// Prints the map with the A* path from the walk-in (W) to the pass (P) drawn as '*'.
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

#include "pathfinding.hpp"

int main(int argc, char** argv) {
  const std::string path = argc > 1 ? argv[1] : "map.txt";
  std::ifstream in(path);
  std::vector<std::string> rows;
  for (std::string line; std::getline(in, line);) {
    if (!line.empty() && line.back() == '\r') line.pop_back();
    if (!line.empty()) rows.push_back(line);
  }
  Grid grid;
  if (!grid.load(rows)) {
    std::cerr << "failed to load map " << path << "\n";
    return 1;
  }
  Point from{}, to{};
  for (int y = 0; y < grid.height; ++y)
    for (int x = 0; x < grid.width; ++x) {
      if (grid.at(x, y) == 'W') from = {x, y};   // walk-in
      if (grid.at(x, y) == 'P') to = {x, y};     // the pass
    }

  auto route = astar(grid, from, to);
  if (route.empty()) {
    std::cerr << "FAIL: no path from walk-in to pass\n";
    return 1;
  }
  for (auto p : route)
    if (grid.rows[p.y][p.x] != 'P') grid.rows[p.y][p.x] = '*';
  for (auto& r : grid.rows) std::cout << r << "\n";
  std::cout << "OK: path length " << route.size() << " (manhattan lower bound "
            << manhattan(from, to) << ")\n";
  return 0;
}
