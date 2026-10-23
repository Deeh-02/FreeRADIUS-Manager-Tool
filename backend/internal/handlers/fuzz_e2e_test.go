package handlers

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every route in main.go, hit with junk as a signed-in super admin: malformed and hostile
// bodies, wrong types, injection strings, absurd numbers, odd path parameters. Whatever comes
// in, the API must answer with a client error or a success, never a server error (5xx) or a
// hang, and must still be running afterwards; nor may it log errors it hid from the caller
// (such as a lost audit row). Routes that touch the outside world by design (network scans,
// router discovery) are left out.

var fuzzSkip = map[string]bool{
	"POST /api/v1/network/scan": true, "POST /api/v1/nas/discover": true, // would scan networks
	"POST /api/v1/restore": true, "POST /api/v1/backup": true, // not junk-sensitive; slow
	"POST /api/v1/auth/logout": true, // ends our own session; tested elsewhere
	"GET /api/v1/live/stats":   true, // an event stream: stays open by design
}

func fuzzRoutes(t *testing.T) [][2]string {
	src, err := os.ReadFile("../../main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	groups := map[string]string{"v1": "/api/v1", "protected": "/api/v1/"}
	gre := regexp.MustCompile(`(\w+)\s*:=\s*(\w+)\.Group\("([^"]*)"\)`)
	for _, m := range gre.FindAllStringSubmatch(text, -1) {
		if base, ok := groups[m[2]]; ok {
			groups[m[1]] = strings.TrimRight(base, "/") + "/" + strings.TrimLeft(m[3], "/")
		}
	}
	rre := regexp.MustCompile(`(\w+)\.(GET|POST|PUT|DELETE|PATCH)\("([^"]*)"`)
	var out [][2]string
	seen := map[string]bool{}
	for _, m := range rre.FindAllStringSubmatch(text, -1) {
		base, ok := groups[m[1]]
		if !ok {
			continue
		}
		p := strings.TrimRight(base, "/")
		if m[3] != "" {
			p += "/" + strings.TrimLeft(m[3], "/")
		}
		key := m[2] + " " + p
		if !seen[key] && !fuzzSkip[key] {
			seen[key] = true
			out = append(out, [2]string{m[2], p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][1]+out[i][0] < out[j][1]+out[j][0] })
	return out
}

func TestStackFuzzEveryRoute(t *testing.T) {
	a, db := needStack(t)
	routes := fuzzRoutes(t)
	if len(routes) < 150 {
		t.Fatalf("found only %d routes in main.go; the route parser needs updating", len(routes))
	}
	// Something for the :id routes to find.
	plan := a.must("POST", "/plans", map[string]interface{}{"name": "Fuzz", "price": 100, "currency": "KES", "validity_days": 30}, 201)
	a.customerWithInvoice(db, "fuzzy", int(plan["id"].(float64)))

	bodies := []string{
		``, `{`, `[]`, `null`, `"x"`, `12`, `{}`,
		`{"id":"1' OR '1'='1","username":"x'; DROP TABLE payments;--","name":"<script>alert(1)</script>"}`,
		`{"amount":1e309,"price":-1,"invoice_id":-5,"plan_id":99999999999,"page":-1,"limit":1000000}`,
		`{"username":["a"],"password":{"x":1},"amount":"lots","status":true,"days":"🙂","phone":"\u0000"}`,
		`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":[[[[[[[[[[1]]]]]]]]]]}}}}}}}}}}`,
		`{"username":"` + strings.Repeat("A", 100000) + `"}`,
		// numbers past PostgreSQL's integer, and dates that are not dates, in the usual fields
		`{"plan_id":2147483648,"user_id":2147483648,"nas_id":2147483648,"invoice_id":2147483648,"days":2147483648,"validity_days":2147483648,"max_uses":2147483648,"quantity":2147483648,"name":"n","username":"fz","price":1e300,"amount":1e300}`,
		`{"valid_from":"2026-99-99","valid_until":"notadate","date":"0000-00-00","expiry":"x","account_expiry":"31/12/2026","from":"","to":"9999-12-31T99:99:99Z","name":"n","username":"fz"}`,
	}
	// One at a time, so one refused value does not hide the others.
	queries := []string{"", "page=-1", "limit=999999", "limit=-5", "page=99999999999999999999", "search=%27%22%3B--", "status=%00",
		"username=%25", "from=notadate", "to=2026-99-99", "days=-1", "days=2147483648", "search=%FF%FE", "sort=1;DROP", "order=sideways"}
	params := []string{"1", "999999", "abc", "-1", "..%2F..%2Fetc%2Fpasswd", "1%27%20OR%201%3D1", "radius_backup_x.sql.gz"}
	pre := regexp.MustCompile(`:\w+`)
	client := &http.Client{Timeout: 20 * time.Second}
	root := strings.Replace(a.base, "/api/v1", "", 1)

	logStart := len(stackLog(t))
	var bad []string
	requests := 0
	for _, r := range routes {
		paths := []string{r[1]}
		if pre.MatchString(r[1]) {
			paths = nil
			for _, p := range params {
				paths = append(paths, pre.ReplaceAllString(r[1], p))
			}
		}
		for _, p := range paths {
			if r[0] == "GET" {
				for _, q := range queries {
					u := p
					if q != "" {
						u += "?" + q
					}
					requests++
					if msg := fuzzOne(client, a, r[0], root, u, ""); msg != "" {
						bad = append(bad, msg)
					}
				}
				continue
			}
			for _, b := range bodies {
				requests++
				if msg := fuzzOne(client, a, r[0], root, p, b); msg != "" {
					bad = append(bad, msg)
				}
			}
		}
	}
	t.Logf("%d routes, %d requests", len(routes), requests)
	if code, _, err := a.do("GET", "/payments", nil, nil); err != nil || code != 200 {
		t.Fatalf("the API is not answering after the fuzzing: %d %v", code, err)
	}
	failIf(t, checkMoney(db))

	// Errors the API hid from the caller but logged: lost audit rows, failed queries.
	seen := map[string]bool{}
	tm := regexp.MustCompile(`"(time|latency)":"[^"]*",?`)
	for _, line := range stackLog(t)[logStart:] {
		if (strings.Contains(line, "Failed to write audit") || strings.Contains(line, `"level":"error"`)) && !strings.Contains(line, "Server error") {
			key := tm.ReplaceAllString(line, "")
			if !seen[key] {
				seen[key] = true
				bad = append(bad, "backend log: "+key)
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		max := len(bad)
		if max > 60 {
			max = 60
		}
		t.Errorf("%d problems:\n  %s", len(bad), strings.Join(bad[:max], "\n  "))
	}
}

// fuzzOne sends one request and describes what is wrong with the answer ("" if nothing).
func fuzzOne(client *http.Client, a *stackAPI, method, root, path, body string) string {
	req, _ := http.NewRequest(method, root+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("%s %s: no answer (%v)", method, path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		a.token = a.login("boss", stackAdminPassword) // a route revoked our session
	}
	if d := time.Since(t0); d > 10*time.Second {
		return fmt.Sprintf("%s %s: took %s", method, path, d.Round(time.Second))
	}
	if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
		if len(body) > 60 {
			body = body[:60] + "…"
		}
		return fmt.Sprintf("%s %s body %q: %d", method, path, body, resp.StatusCode)
	}
	return ""
}

// stackLog is the local stack backend's log, as lines (empty against a remote API).
func stackLog(t *testing.T) []string {
	stack := os.Getenv("TEST_STACK")
	if stack == "" {
		return nil
	}
	out, err := exec.Command(stack, "log", "100000000").Output()
	if err != nil {
		t.Logf("reading the backend log: %v", err)
	}
	return strings.Split(string(out), "\n")
}
