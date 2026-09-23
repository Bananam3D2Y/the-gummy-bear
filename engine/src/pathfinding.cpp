#include "pathfinding.hpp"

#include <algorithm>
#include <climits>
#include <functional>
#include <queue>
#include <utility>

namespace {
// Tiles agents cannot walk through: trees/walls, water, buildings, rocks.
bool is_solid(char c) { return c == '#' || c == '~' || c == 'B' || c == '^'; }
}  // namespace

bool Grid::load(const std::vector<std::string>& in) {
  if (in.empty()) return false;
  rows = in;
  height = static_cast<int>(rows.size());
  width = static_cast<int>(rows[0].size());
  for (const auto& r : rows) {
    if (static_cast<int>(r.size()) != width) return false;  // map must be rectangular
  }
  return true;
}

bool Grid::walkable(int x, int y) const {
  return in_bounds(x, y) && !is_solid(rows[y][x]);
}

std::vector<Point> astar(const Grid& grid, Point start, Point goal,
                         const std::unordered_set<int>* blocked) {
  if (start == goal || !grid.walkable(goal.x, goal.y)) return {};

  const int n = grid.width * grid.height;
  const int si = grid.index(start.x, start.y);
  const int gi = grid.index(goal.x, goal.y);

  std::vector<int> g_score(n, INT_MAX);  // cost of cheapest known path to each tile
  std::vector<int> came_from(n, -1);     // breadcrumb for reconstructing the path
  std::vector<char> closed(n, 0);

  // Min-heap ordered by f = g + h
  using Node = std::pair<int, int>;  // (f_score, tile index)
  std::priority_queue<Node, std::vector<Node>, std::greater<Node>> open;

  g_score[si] = 0;
  open.push({manhattan(start, goal), si});

  static const int dx[4] = {1, -1, 0, 0};
  static const int dy[4] = {0, 0, 1, -1};

  while (!open.empty()) {
    const int cur = open.top().second;
    open.pop();
    if (cur == gi) break;
    if (closed[cur]) continue;  // stale heap entry
    closed[cur] = 1;

    const int cx = cur % grid.width;
    const int cy = cur / grid.width;
    for (int d = 0; d < 4; ++d) {
      const int nx = cx + dx[d];
      const int ny = cy + dy[d];
      if (!grid.walkable(nx, ny)) continue;
      const int ni = grid.index(nx, ny);
      if (blocked && ni != gi && blocked->count(ni)) continue;
      const int tentative = g_score[cur] + 1;
      if (tentative < g_score[ni]) {
        g_score[ni] = tentative;
        came_from[ni] = cur;
        open.push({tentative + manhattan({nx, ny}, goal), ni});
      }
    }
  }

  if (g_score[gi] == INT_MAX) return {};

  std::vector<Point> path;
  for (int cur = gi; cur != si; cur = came_from[cur]) {
    path.push_back({cur % grid.width, cur / grid.width});
  }
  std::reverse(path.begin(), path.end());
  return path;
}
