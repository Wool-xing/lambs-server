#!/usr/bin/env python3
# Lambs node agent — exposes host metrics for the Lambs management server.
# stdlib only. Polled every 30s by lambs-server (go).
import http.server
import json
import os
import time

BOOT = time.time()


def cpu_percent():
    with open('/proc/stat') as f:
        v1 = [int(x) for x in f.readline().split()[1:8]]
    idle1, total1 = v1[3] + v1[4], sum(v1)
    time.sleep(0.2)
    with open('/proc/stat') as f:
        v2 = [int(x) for x in f.readline().split()[1:8]]
    idle2, total2 = v2[3] + v2[4], sum(v2)
    if total2 == total1:
        return 0.0
    return round((1 - (idle2 - idle1) / (total2 - total1)) * 100, 1)


def mem():
    info = {}
    with open('/proc/meminfo') as f:
        for line in f:
            if ':' not in line:
                continue
            k, v = line.split(':')
            info[k] = int(v.split()[0]) * 1024
    total = info['MemTotal']
    return total, total - info['MemAvailable']


def disk():
    st = os.statvfs('/')
    total = st.f_blocks * st.f_frsize
    free = st.f_bavail * st.f_frsize
    return total - free, total


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != '/health':
            self.send_response(404)
            self.end_headers()
            return
        used, total = disk()
        mt, mu = mem()
        body = json.dumps({
            'hostname': os.uname().nodename,
            'cpu_percent': cpu_percent(),
            'memory_used_mb': mu // 1048576,
            'memory_total_mb': mt // 1048576,
            'disk_used_gb': round(used / 1073741824, 1),
            'disk_total_gb': round(total / 1073741824, 1),
            'uptime_seconds': int(time.time() - BOOT),
        }).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == '__main__':
    http.server.ThreadingHTTPServer(('100.126.18.126', 3901), Handler).serve_forever()
