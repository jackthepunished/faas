package reposcan

// Detector file names and manifest keys. Centralized here so the
// goconst lint doesn't flag a path string every time a detector
// probes the same file. These are the only names Scan() will
// open; adding a new detector means adding a new entry here.
const (
	// Detector file names (case-sensitive on case-sensitive filesystems).
	nameComposeYAML       = "compose.yaml"
	nameComposeYML        = "compose.yml"
	nameDockerComposeYML  = "docker-compose.yml"
	nameDockerComposeYAML = "docker-compose.yaml"
	nameProcfile          = "Procfile"
	nameFlyTOML           = "fly.toml"
	namePackageJSON       = "package.json"
	namePnpmWorkspace     = "pnpm-workspace.yaml"
	nameTurboJSON         = "turbo.json"
	nameNxJSON            = "nx.json"
	nameGoWork            = "go.work"
	nameGoWorkSum         = "go.work.sum"
	nameDockerfile        = "Dockerfile"
	nameDockerfileLower   = "dockerfile"
	nameK8s               = "k8s"
	nameKubernetes        = "kubernetes"
	nameDeploy            = "deploy"
	nameManifests         = "manifests"
	nameAppYAML           = "app.yaml"
	nameAppYML            = "app.yml"
	nameRenderYAML        = "render.yaml"
	nameRenderYML         = "render.yml"
	nameServerlessYML     = "serverless.yml"
	nameServerlessYAML    = "serverless.yaml"

	// Manifest key names.
	keyApp       = "app"
	keyName      = "name"
	keyWeb       = "web"
	keyWorker    = "worker"
	keyCron      = "cron"
	keyRelease   = "release"
	keyConsumer  = "consumer"
	keyClock     = "clock"
	keyScheduler = "scheduler"

	// Env-var hints returned to the confirm table.
	hintDatabaseURL      = "DATABASE_URL"
	hintRedisURL         = "REDIS_URL"
	hintMongoURL         = "MONGODB_URL"
	hintCassandraURL     = "CASSANDRA_URL"
	hintClickhouseURL    = "CLICKHOUSE_URL"
	hintElasticsearchURL = "ELASTICSEARCH_URL"
	hintOpensearchURL    = "OPENSEARCH_URL"
	hintRabbitmqURL      = "RABBITMQ_URL"
	hintKafkaURL         = "KAFKA_URL"
	hintNatsURL          = "NATS_URL"
	hintMinioURL         = "MINIO_URL"
	hintMemcachedURL     = "MEMCACHED_URL"
	hintEtcdURL          = "ETCD_URL"
)
