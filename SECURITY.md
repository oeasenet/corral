# Security

corral and, by default, the runners it manages have access to the host's Docker socket, which is
root-equivalent on that host. Treat the runner host as part of your CI trust boundary and only run
trusted workflows on it. The dashboard is unauthenticated unless `ADMIN_PASSWORD` is set — keep it
behind your firewall or bind it to `127.0.0.1`.

## Reporting a vulnerability

Please do not open a public issue for security problems. Use GitHub's private vulnerability
reporting on this repository ("Security" → "Report a vulnerability"). You will get an
acknowledgement within a few days; fixes ship as a new release and are noted in the release notes.

## Supported versions

The latest release (`ghcr.io/oeasenet/corral:latest`) is supported; older tags receive no fixes.
