# ecs-auto-draining

EC2 AutoScale の ScaleIn 時に ECS Container Instance を Draining にしすべてのタスクが終了するのを待つ


## Requirements

```bash
brew install go
brew tap aws/tap
brew install aws-sam-cli
```


## Deployment

```bash
sam build
sam deploy --guided
```


## Local development

```bash
golangci-lint run
sam local invoke Function -e event.json
```
