package gpu

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Point struct {
	At          float64            `json:"at"`
	Utilization *float64           `json:"utilization"`
	Observed    float64            `json:"observed_seconds"`
	Owners      map[string]float64 `json:"owners"`
	weighted    float64
	measured    float64
}
type Series struct {
	UUID   string   `json:"uuid"`
	Name   string   `json:"name"`
	Points []*Point `json:"points"`
}
type UserUsage struct {
	Owner   string  `json:"owner"`
	Seconds float64 `json:"seconds"`
}
type History struct {
	From   float64     `json:"from"`
	To     float64     `json:"to"`
	Step   float64     `json:"step"`
	Series []*Series   `json:"series"`
	Users  []UserUsage `json:"users"`
	Since  *float64    `json:"since"`
}

func IsRoute(path string) bool { return strings.HasPrefix(path, "/api/gpu/") }
func (m *Monitor) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if r.Method != "GET" || r.URL.Path != "/api/gpu/overview" {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	now := platform.Now()
	q := r.URL.Query()
	parse := func(key string, def, min, max float64) (float64, error) {
		raw := q.Get(key)
		if raw == "" {
			return def, nil
		}
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min || n > max {
			return 0, httpapi.NewError(400, "无效的 GPU 时间参数: "+key)
		}
		return n, nil
	}
	hours, err := parse("hours", 6, .25, 72)
	if err != nil {
		return 0, nil, err
	}
	end, err := parse("end", now, now-retention.Seconds(), now+60)
	if err != nil {
		return 0, nil, err
	}
	end = math.Min(end, now)
	step, err := parse("step", 60, 15, 3600)
	if err != nil {
		return 0, nil, err
	}
	// At most 864 buckets per GPU, regardless of requested zoom or granularity.
	step = math.Max(step, math.Ceil(hours*3600/864/15)*15)
	from := math.Max(end-hours*3600, now-retention.Seconds())
	h, err := m.history(now, from, end, step)
	if err != nil {
		return 0, nil, err
	}
	m.mu.RLock()
	snapshot := m.current
	m.mu.RUnlock()
	return 200, map[string]any{"current": snapshot, "history": h, "now": now, "sample_seconds": samplePeriod.Seconds()}, nil
}
func (m *Monitor) history(now, from, to, step float64) (History, error) {
	h := History{From: from, To: to, Step: step, Series: []*Series{}, Users: []UserUsage{}}
	rows, err := m.db.SQL.Query("SELECT gpu_uuid,name,started_at,ended_at,utilization,owners FROM gpu_intervals WHERE ended_at>? ORDER BY ended_at,gpu_uuid", now-retention.Seconds())
	if err != nil {
		return h, err
	}
	defer rows.Close()
	series := map[string]*Series{}
	buckets := map[string]map[int]*Point{}
	users := map[string]float64{}
	for rows.Next() {
		var id, name, raw string
		var start, end float64
		var util sql.NullFloat64
		if err = rows.Scan(&id, &name, &start, &end, &util, &raw); err != nil {
			return h, err
		}
		var owners []string
		if err = json.Unmarshal([]byte(raw), &owners); err != nil {
			return h, fmt.Errorf("GPU 历史归属格式无效: %w", err)
		}
		start = math.Max(start, now-retention.Seconds())
		end = math.Min(end, now)
		if end <= start {
			continue
		}
		if h.Since == nil || start < *h.Since {
			v := start
			h.Since = &v
		}
		for _, owner := range owners {
			users[owner] += end - start
		}
		a, b := math.Max(start, from), math.Min(end, to)
		if b <= a {
			continue
		}
		if series[id] == nil {
			series[id] = &Series{UUID: id, Name: name, Points: []*Point{}}
			buckets[id] = map[int]*Point{}
		}
		for a < b {
			index := int(math.Floor((a - from) / step))
			bucketEnd := math.Min(b, from+float64(index+1)*step)
			seconds := bucketEnd - a
			if seconds <= 0 {
				break
			}
			p := buckets[id][index]
			if p == nil {
				p = &Point{At: from + float64(index)*step, Owners: map[string]float64{}}
				buckets[id][index] = p
			}
			p.Observed += seconds
			if util.Valid {
				p.weighted += util.Float64 * seconds
				p.measured += seconds
			}
			for _, owner := range owners {
				p.Owners[owner] += seconds
			}
			a = bucketEnd
		}
	}
	if err = rows.Err(); err != nil {
		return h, err
	}
	for id, s := range series {
		for _, p := range buckets[id] {
			if p.measured > 0 {
				n := p.weighted / p.measured
				p.Utilization = &n
			}
			s.Points = append(s.Points, p)
		}
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].At < s.Points[j].At })
		h.Series = append(h.Series, s)
	}
	sort.Slice(h.Series, func(i, j int) bool { return h.Series[i].UUID < h.Series[j].UUID })
	for owner, seconds := range users {
		h.Users = append(h.Users, UserUsage{owner, seconds})
	}
	sort.Slice(h.Users, func(i, j int) bool {
		if h.Users[i].Seconds == h.Users[j].Seconds {
			return h.Users[i].Owner < h.Users[j].Owner
		}
		return h.Users[i].Seconds > h.Users[j].Seconds
	})
	return h, nil
}
