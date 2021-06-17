cd /Users/wujiangfa/go/gopath/src/github.com/wujiangfa-xlauncher/helm-operator/docker
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o helm-operator ../cmd/helm-operator/main.go
image="registry.cn-hangzhou.aliyuncs.com/launcher-agent-only/helm-operator:v1"
docker build -t $image .
docker push $image
docker rmi $image
