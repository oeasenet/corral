# GitHub Runner KMS Service (Go Implementation)

A Key Management Service (KMS) for GitHub Actions Runners that dynamically generates runner registration tokens from Personal Access Tokens (PATs), eliminating the need to expose PATs inside runner containers.

## Features

- **Enhanced Security**: Keeps PATs secure outside of runner containers
- **Multiple Endpoints**: Support for both organization and repository-level runners
- **Web UI Dashboard**: Built-in monitoring interface with real-time statistics
- **REST API**: JSON endpoints for integration and monitoring
- **Statistics Tracking**: Request counts, success rates, and usage by org/repo
- **Health Checks**: Built-in health endpoint for container orchestration
- **Concurrent Safe**: Thread-safe operations with proper mutex handling
- **Docker Ready**: Multi-stage Docker build for minimal container size

## Configuration

The service can be configured using either environment variables or a `config.json` file.

### Environment Variables

Set PATs as environment variables with the prefix `PAT_`:
```bash
export PAT_myorg=ghp_yourPersonalAccessTokenHere
export PAT_anotherorg=ghp_anotherPersonalAccessTokenHere
```

### Configuration File

Create a `config.json` file:
```json
{
  "your-github-org": "ghp_yourPersonalAccessTokenHere",
  "another-org": "ghp_anotherPersonalAccessTokenHere"
}
```

## Running the Service

### Local Development

```bash
# Install dependencies
go mod download

# Run the server
go run kms.go

# Or build and run
go build -o kms-server
./kms-server
```

### Using Docker

```bash
# Build the image
docker build -t github-runner-kms .

# Run with environment variables
docker run -p 3000:3000 \
  -e PAT_myorg=ghp_token1 \
  -e PAT_anotherorg=ghp_token2 \
  github-runner-kms

# Or with config file
docker run -p 3000:3000 \
  -v $(pwd)/config.json:/root/config.json \
  github-runner-kms
```

## API Endpoints

### Runner Token Endpoints

#### Repository Registration Token
```
GET /repo/{owner}/{repo}/registration-token
```
Returns a registration token for a specific repository runner.

#### Repository Remove Token
```
GET /repo/{owner}/{repo}/remove-token
```
Returns a remove token for a specific repository runner.

#### Organization Registration Token
```
GET /{org}/registration-token
```
Returns a registration token for an organization runner.

#### Organization Remove Token
```
GET /{org}/remove-token
```
Returns a remove token for an organization runner.

### Monitoring Endpoints

#### Web UI Dashboard
```
GET /
```
Interactive web dashboard showing service statistics and configuration.

#### Statistics API
```
GET /api/stats
```
Returns JSON statistics including request counts and success rates.

#### Configuration API
```
GET /api/config
```
Returns configured organizations (PAT values are not exposed).

#### Health Check
```
GET /health
```
Returns service health status for container orchestration.

## Web UI Features

The built-in web UI provides:
- Real-time service status
- Request statistics and success rates
- Configured organizations list
- API endpoint documentation
- Auto-refresh every 30 seconds

Access the UI at `http://localhost:3000/` when the service is running.

## Environment Variables

- `PORT`: Server port (default: 3000)
- `PAT_*`: Personal Access Tokens for organizations/users

## Security Considerations

1. **Never commit PATs to version control**
2. **Use environment variables or secure secret management**
3. **Limit PAT scopes to minimum required permissions**
4. **Run the service in a secure network environment**
5. **Consider using TLS/HTTPS in production**

## Integration with GitHub Runners

This service is designed to work with self-hosted GitHub Actions runners. Instead of passing PATs directly to runner containers, runners can request tokens from this service:

```bash
# Example runner registration
TOKEN=$(curl -s http://kms-service:3000/myorg/registration-token)
./config.sh --url https://github.com/myorg --token $TOKEN
```

## Differences from Node.js Version

This Go implementation includes several enhancements:
- **Web UI Dashboard**: Visual monitoring interface
- **Better Statistics**: Detailed request tracking
- **Health Checks**: Container-ready health endpoints
- **Structured Logging**: Clear, informative log messages
- **Type Safety**: Go's type system prevents many runtime errors
- **Lower Resource Usage**: Compiled binary with minimal overhead
- **Embedded Templates**: Single binary deployment with UI included

## License

GPL (same as original Node.js implementation)