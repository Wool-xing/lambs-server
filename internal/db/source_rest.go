package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RESTSource implements DataSource for projects exposing a REST API.
// DSN format: http(s)://host:port/api (base URL, no trailing slash).
//
// Conventions the managed project must follow:
//   GET    {base}/            → JSON array of resource names, or {"collections": [...]}
//   GET    {base}/{resource}  → JSON array of objects, or {"rows": [...]}
//   POST   {base}/{resource}  → create (request body = object)
//   PUT    {base}/{resource}/{pk}   → update (request body = object)
//   DELETE {base}/{resource}/{pk}   → delete
type RESTSource struct {
	dsn string
}

// restHTTPClient is shared across all REST datasources — a fresh client per
// request drops keep-alive and TLS session reuse (R12 perf review).
var restHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (s *RESTSource) base() string {
	return strings.TrimSuffix(s.dsn, "/")
}

func (s *RESTSource) do(method, url string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := restHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func (s *RESTSource) ListCollections() ([]string, error) {
	data, status, err := s.do("GET", s.base()+"/", nil)
	if err != nil {
		return []string{}, err
	}
	if status >= 500 {
		return []string{}, fmt.Errorf("rest api error: status %d", status)
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		if arr == nil {
			arr = []string{}
		}
		return arr, nil
	}
	var wrapper struct {
		Collections []string `json:"collections"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Collections != nil {
		return wrapper.Collections, nil
	}
	return []string{}, nil
}

// fetchRows GETs the collection and parses rows (shared by ReadItems and
// CountItems — REST has no dedicated count endpoint, so counting means
// fetching).
func (s *RESTSource) fetchRows(collection string) ([]map[string]interface{}, error) {
	data, status, err := s.do("GET", s.base()+"/"+collection, nil)
	if err != nil {
		return nil, err
	}
	if status >= 500 {
		return nil, fmt.Errorf("rest api error: status %d", status)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		var wrapper struct {
			Rows []map[string]interface{} `json:"rows"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			return nil, fmt.Errorf("invalid rest response: %w", err)
		}
		rows = wrapper.Rows
	}
	return rows, nil
}

func (s *RESTSource) ReadItems(collection string, limit, offset int) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	rows, err := s.fetchRows(collection)
	if err != nil {
		return nil, nil, "", err
	}
	colSet := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		// Same column redaction as the SQL sources: never expose
		// password/token columns through the data browser.
		if strings.Contains(strings.ToLower(c), "password") || strings.Contains(strings.ToLower(c), "token") {
			continue
		}
		cols = append(cols, c)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	// Client-side paging: the REST API has no server-side pagination contract.
	if limit > 0 && offset < len(rows) {
		end := offset + limit
		if end > len(rows) {
			end = len(rows)
		}
		rows = rows[offset:end]
	} else if limit > 0 {
		rows = []map[string]interface{}{}
	}
	// Honest pk detection: only claim "id" when rows actually carry it.
	// Otherwise the UI must show "无主键" instead of breaking edits.
	pk := ""
	if len(rows) > 0 {
		if _, ok := rows[0]["id"]; ok {
			pk = "id"
		}
	}
	return rows, cols, pk, nil
}

func (s *RESTSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	body, _ := json.Marshal(data)
	_, status, err := s.do("POST", s.base()+"/"+collection, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("rest api error: status %d", status)
	}
	return nil
}

func (s *RESTSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	body, _ := json.Marshal(data)
	_, status, err := s.do("PUT", s.base()+"/"+collection+"/"+url.PathEscape(pkVal), body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("rest api error: status %d", status)
	}
	return nil
}

func (s *RESTSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	_, status, err := s.do("DELETE", s.base()+"/"+collection+"/"+url.PathEscape(pkVal), nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("rest api error: status %d", status)
	}
	return nil
}

func (s *RESTSource) CountItems(collection string) (int, error) {
	rows, err := s.fetchRows(collection)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}
