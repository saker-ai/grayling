apiVersion: v1
kind: Pod
metadata:
  name: golab-__NAME__
  namespace: golab-issue13
  labels: {app: golab, replica: __NAME__}
spec:
  nodeSelector: {kubernetes.io/hostname: golab-issue13-__NODE__}
  containers:
    - name: golab
      image: golab:issue13-local
      imagePullPolicy: Never
      args:
        - -addr=0.0.0.0:8688
        - -pg=postgres://golab:__PG_PASSWORD__@postgres:5432/golab?sslmode=disable
        - -redis=redis:6379
        - -auth-secret-file=/run/secrets/auth-secret
        - -gonacos-addr=127.0.0.1:18848
        - -gonacos-grpc-addr=127.0.0.1:19848
        - -nacos-native-auth=true
        - -rollout-interval=0
      volumeMounts:
        - {name: auth-secret, mountPath: /run/secrets, readOnly: true}
    - name: tls-l4-proxy
      image: golab-native-proxy:issue13-local
      imagePullPolicy: Never
      ports:
        - {name: native-http, containerPort: 8848}
        - {name: native-grpc, containerPort: 9848}
      volumeMounts:
        - {name: tls, mountPath: /tls, readOnly: true}
  volumes:
    - name: auth-secret
      secret: {secretName: issue13-secrets, defaultMode: 384, items: [{key: auth-secret, path: auth-secret}]}
    - name: tls
      secret: {secretName: native-tls, defaultMode: 256}
---
apiVersion: v1
kind: Service
metadata: {name: golab-__NAME__, namespace: golab-issue13}
spec:
  selector: {app: golab, replica: __NAME__}
  ports:
    - {name: api, port: 8688, targetPort: 8688}
    - {name: native-http, port: 8848, targetPort: native-http}
    - {name: native-grpc, port: 9848, targetPort: native-grpc}
