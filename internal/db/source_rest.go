package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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

func (s *RESTSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	data, status, err := s.do("GET", s.base()+"/"+collection, nil)
	if err != nil {
		return nil, nil, "", err
	}
	if status >= 500 {
		return nil, nil, "", fmt.Errorf("rest api error: status %d", status)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		var wrapper struct {
			Rows []map[string]interface{} `json:"rows"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			return nil, nil, "", fmt.Errorf("invalid rest response: %w", err)
		}
		rows = wrapper.Rows
	}
	colSet := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return rows, cols, "id", nil
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
	_, status, err := s.do("PUT", s.base()+"/"+collection+"/"+pkVal, body)
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
	_, status, err := s.do("DELETE", s.base()+"/"+collection+"/"+pkVal, nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("rest api error: status %d", status)
	}
	return nil
}
