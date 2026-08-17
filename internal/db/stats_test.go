package db

import "testing"

func TestBuildStatCardsSQL(t *testing.T) {
	cards := BuildStatCards("MySQL", map[string]interface{}{"tables": 7, "rows": 1234})
	if len(cards) != 3 {
		t.Fatalf("want 3 cards, got %d: %v", len(cards), cards)
	}
	if cards[0]["label"] != "表数量" || cards[0]["value"] != 7 {
		t.Errorf("card0 = %v, want 表数量=7", cards[0])
	}
	if cards[1]["label"] != "数据行数" || cards[1]["value"] != 1234 {
		t.Errorf("card1 = %v, want 数据行数=1234", cards[1])
	}
	if cards[2]["label"] != "数据库" || cards[2]["value"] != "MySQL" {
		t.Errorf("card2 = %v, want 数据库=MySQL", cards[2])
	}
}

func TestBuildStatCardsMongo(t *testing.T) {
	cards := BuildStatCards("MongoDB（文档型）", map[string]interface{}{"collections": 3, "documents": 9000, "storage_mb": 5.2})
	if len(cards) != 3 {
		t.Fatalf("want 3 cards, got %d", len(cards))
	}
	if cards[0]["label"] != "集合数" || cards[0]["value"] != 3 {
		t.Errorf("card0 = %v", cards[0])
	}
	if cards[1]["label"] != "文档数" || cards[1]["value"] != 9000 {
		t.Errorf("card1 = %v", cards[1])
	}
	if cards[2]["label"] != "存储占用" || cards[2]["value"] != "5.2 MB" {
		t.Errorf("card2 = %v", cards[2])
	}
}

func TestBuildStatCardsRedis(t *testing.T) {
	cards := BuildStatCards("Redis（KV型）", map[string]interface{}{"keys": 42, "memory": "1.2M", "uptime_sec": 3600})
	if len(cards) != 3 {
		t.Fatalf("want 3 cards, got %d", len(cards))
	}
	if cards[0]["label"] != "键数量" || cards[0]["value"] != 42 {
		t.Errorf("card0 = %v", cards[0])
	}
	if cards[1]["label"] != "内存占用" || cards[1]["value"] != "1.2M" {
		t.Errorf("card1 = %v", cards[1])
	}
	if cards[2]["label"] != "运行时长" || cards[2]["value"] != "1小时0分" {
		t.Errorf("card2 = %v", cards[2])
	}
}

func TestBuildStatCardsREST(t *testing.T) {
	cards := BuildStatCards("REST API", map[string]interface{}{"status": "healthy", "latency_ms": 12})
	if len(cards) != 2 {
		t.Fatalf("want 2 cards, got %d", len(cards))
	}
	if cards[0]["label"] != "健康状态" || cards[0]["value"] != "正常" {
		t.Errorf("card0 = %v", cards[0])
	}
	if cards[1]["label"] != "响应延迟" || cards[1]["value"] != "12 ms" {
		t.Errorf("card1 = %v", cards[1])
	}
}

func TestBuildStatCardsUnknownType(t *testing.T) {
	if cards := BuildStatCards("SomethingElse", map[string]interface{}{}); cards != nil {
		t.Errorf("unknown type should return nil, got %v", cards)
	}
}

func TestTypeKind(t *testing.T) {
	cases := map[string]string{
		"MySQL":        "sql",
		"PostgreSQL":   "sql",
		"直连 SQLite":    "sql",
		"MongoDB（文档型）": "mongo",
		"Redis（KV型）":   "redis",
		"REST API":     "rest",
		"whatever":     "",
	}
	for in, want := range cases {
		if got := typeKind(in); got != want {
			t.Errorf("typeKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedisInfoParsing(t *testing.T) {
	info := "# Memory\r\nused_memory:1000\r\nused_memory_human:1.02K\r\n# Server\r\nuptime_in_seconds: 3721\r\n"
	if got := redisInfoStr(info, "used_memory_human"); got != "1.02K" {
		t.Errorf("used_memory_human = %q, want 1.02K", got)
	}
	if got := redisInfoInt(info, "uptime_in_seconds"); got != 3721 {
		t.Errorf("uptime_in_seconds = %d, want 3721", got)
	}
	if got := redisInfoInt(info, "missing_field"); got != 0 {
		t.Errorf("missing field should be 0, got %d", got)
	}
	if got := redisInfoInt("uptime_in_seconds:abc", "uptime_in_seconds"); got != 0 {
		t.Errorf("malformed value should be 0, got %d", got)
	}
}
