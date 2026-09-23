#pragma once
#include <string>
#include <unordered_set>
#include <vector>

struct Point {
  int x = 0;
  int y = 0;
  bool operator==(const Point& o) const { return x == o.x && y == o.y; }
  bool operator!=(const Point& o) const { return !(*this == o); }
};

// Manhattan distance: the A* heuristic for 4-way movement.
inline int manhattan(Point a, Point b) {
  return (a.x > b.x ? a.x - b.x : b.x - a.x) + (a.y > b.y ? a.y - b.y : b.y - a.y);
}

// Chebyshev distance: "within N tiles" in any direction, used for "is the agent at the forge?"
inline int chebyshev(Point a, Point b) {
  int dx = a.x > b.x ? a.x - b.x : b.x - a.x;
  int dy = a.y > b.y ? a.y - b.y : b.y - a.y;
  return dx > dy ? dx : dy;
}

class Grid {
 public:
  bool load(const std::vector<std::string>& rows);
  bool walkable(int x, int y) const;
  bool in_bounds(int x, int y) const { return x >= 0 && y >= 0 && x < width && y < height; }
  int index(int x, int y) const { return y * width + x; }
  char at(int x, int y) const { return rows[y][x]; }

  int width = 0;
  int height = 0;
  std::vector<std::string> rows;
};

// Returns the path from start to goal, EXCLUDING start and INCLUDING goal.
// Empty result means "no path" (or start == goal).
// `blocked` holds tile indices to treat as walls (e.g. other agents); the goal itself is never blocked.
std::vector<Point> astar(const Grid& grid, Point start, Point goal,
                         const std::unordered_set<int>* blocked = nullptr);
