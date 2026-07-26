--
-- PostgreSQL database dump
--



SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: citext; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;


--
-- Name: EXTENSION citext; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION citext IS 'data type for case-insensitive character strings';


--
-- Name: apps_egress_allowlist_cidr_check(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.apps_egress_allowlist_cidr_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
declare
  bad cidr;
begin
  if new.egress_allowlist is null or cardinality(new.egress_allowlist) = 0 then
    return new;
  end if;
  -- Per-entry guards: family must be v4 or v6, mask must be non-zero.
  -- The /0 reject closes the same hole as the v4-only trigger's
  -- `prefix.Bits() == 0` reject at the wire + apid layers: an
  -- operator cannot pin "the entire address space" — that is the
  -- chain-policy accept's job, not the allowlist's. Two narrow
  -- selects (one per guard) keep the error messages specific; a
  -- combined select with bool_or would conflate family and masklen
  -- failures and force a parser to guess.
  for bad in
    select c
      from unnest(new.egress_allowlist) c
     where family(c) not in (4, 6)
     limit 1
  loop
    raise exception 'apps_egress_allowlist: only v4 or v6 CIDRs (got family % for %)', family(bad), bad
      using errcode = '23514',
            constraint = 'apps_egress_allowlist_cidr';
  end loop;
  for bad in
    select c
      from unnest(new.egress_allowlist) c
     where masklen(c) = 0
     limit 1
  loop
    raise exception 'apps_egress_allowlist: rejected % (masklen /0; ADR-032 non-/0 contract)', bad
      using errcode = '23514',
            constraint = 'apps_egress_allowlist_cidr';
  end loop;
  return new;
end;
$$;


--
-- Name: compute_node_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.compute_node_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
declare
    payload jsonb;
begin
    payload := jsonb_build_object(
        'node_id', new.id::text,
        'active', new.active
    );
    perform pg_notify('compute_node_changed', payload::text);
    return new;
end;
$$;


--
-- Name: instances_started_at_set(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.instances_started_at_set() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
begin
  if new.started_at is null then
    new.started_at = now();
  end if;
  return new;
end
$$;


--
-- Name: invocation_done_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.invocation_done_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
declare
    payload jsonb;
begin
    if (tg_op = 'UPDATE' and old.state in ('pending','dispatching')
        and new.state in ('completed','failed','cancelled')) then
        payload := jsonb_build_object(
            'invocation_id', new.id::text,
            'app_id', new.app_id::text,
            'source', new.source,
            'state', new.state
        );
        perform pg_notify('invocation_done', payload::text);
    end if;
    return new;
end;
$$;


--
-- Name: invocation_due_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.invocation_due_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
declare
    payload jsonb;
begin
    payload := jsonb_build_object(
        'invocation_id', new.id::text,
        'app_id', new.app_id::text,
        'source', new.source
    );
    perform pg_notify('invocation_due', payload::text);
    return new;
end;
$$;


SET default_table_access_method = heap;

--
-- Name: account_passwords; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_passwords (
    account_id uuid NOT NULL,
    hash text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    email public.citext NOT NULL,
    plan text DEFAULT 'free'::text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    provider_customer_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deletion_requested_at timestamp with time zone,
    stripe_subscription_item text,
    last_quota_warning_at timestamp with time zone,
    past_due_at timestamp with time zone,
    CONSTRAINT accounts_plan_check CHECK ((plan = ANY (ARRAY['free'::text, 'hobby'::text, 'pro'::text, 'scale'::text]))),
    CONSTRAINT accounts_status_check CHECK ((status = ANY (ARRAY['active'::text, 'past_due'::text, 'suspended'::text, 'deleted_pending'::text])))
);


--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_keys (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    account_id uuid NOT NULL,
    key_sha256 bytea NOT NULL,
    label text,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    scopes text[] DEFAULT '{admin}'::text[] NOT NULL,
    CONSTRAINT api_keys_scopes_vocab_chk CHECK (((scopes <@ ARRAY['admin'::text, 'deploy:write'::text, 'secrets:read'::text, 'secrets:write'::text, 'usage:read'::text, 'apps:read'::text]) AND (cardinality(scopes) > 0)))
);


--
-- Name: app_secrets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_secrets (
    account_id uuid NOT NULL,
    app_id uuid NOT NULL,
    key text NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT app_secrets_key_shape CHECK (((key ~ '^[A-Z][A-Z0-9_]*$'::text) AND (length(key) <= 128)))
);


--
-- Name: apps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.apps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    account_id uuid NOT NULL,
    slug text NOT NULL,
    type text DEFAULT 'app'::text NOT NULL,
    runtime text,
    ram_mb integer NOT NULL,
    idle_timeout_s integer,
    max_concurrency integer DEFAULT 1 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    manifest jsonb DEFAULT '{}'::jsonb NOT NULL,
    github_install_id bigint,
    github_repo_full_name text,
    github_production_branch text,
    min_instances integer DEFAULT 0 NOT NULL,
    egress_allowlist cidr[] DEFAULT '{}'::cidr[] NOT NULL,
    autoscale_target_rps integer,
    autoscale_target_cpu_pct integer,
    CONSTRAINT apps_autoscale_target_cpu_pct_range CHECK (((autoscale_target_cpu_pct IS NULL) OR ((autoscale_target_cpu_pct >= 0) AND (autoscale_target_cpu_pct <= 100)))),
    CONSTRAINT apps_autoscale_target_rps_nonneg CHECK (((autoscale_target_rps IS NULL) OR (autoscale_target_rps >= 0))),
    CONSTRAINT apps_idle_timeout_s_check CHECK (((idle_timeout_s IS NULL) OR (idle_timeout_s >= 10))),
    CONSTRAINT apps_max_concurrency_check CHECK ((max_concurrency >= 1)),
    CONSTRAINT apps_min_instances_check CHECK ((min_instances >= 0)),
    CONSTRAINT apps_ram_mb_check CHECK ((ram_mb > 0)),
    CONSTRAINT apps_runtime_check CHECK (((runtime IS NULL) OR (runtime = ANY (ARRAY['node22'::text, 'python312'::text, 'go124'::text, 'go124-alpine'::text])))),
    CONSTRAINT apps_status_check CHECK ((status = ANY (ARRAY['active'::text, 'evicted_cold'::text, 'deleted'::text]))),
    CONSTRAINT apps_type_check CHECK ((type = ANY (ARRAY['app'::text, 'function'::text])))
);


--
-- Name: COLUMN apps.autoscale_target_rps; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.autoscale_target_rps IS 'Per-instance RPS target. When live_request_count / live_instance_count exceeds this, schedd admits another instance (up to plan max_concurrency). Hobby/Pro/Scale only (plan gate). 0 / NULL = disabled (the trigger skips the app).';


--
-- Name: COLUMN apps.autoscale_target_cpu_pct; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.autoscale_target_cpu_pct IS 'Per-instance CPU% target (1..100). Pro/Scale only (plan gate). 0 / NULL = disabled (the trigger skips the app). CPU target is unbounded above 100 inside the DB; the apid handler enforces [1, 100] via 422.';


--
-- Name: builds; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.builds (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    deployment_id uuid NOT NULL,
    kind text NOT NULL,
    source_bytes bigint NOT NULL,
    status text NOT NULL,
    failure_class text,
    log_path text,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    enqueued_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT builds_failure_class_check CHECK (((failure_class IS NULL) OR (failure_class = ANY (ARRAY['oom'::text, 'timeout'::text, 'user_error'::text, 'infra'::text])))),
    CONSTRAINT builds_kind_check CHECK ((kind = ANY (ARRAY['railpack'::text, 'dockerfile'::text, 'tarball'::text]))),
    CONSTRAINT builds_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text])))
);


--
-- Name: cli_auth_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cli_auth_codes (
    token_hash bytea NOT NULL,
    account_id uuid,
    status text DEFAULT 'pending'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT cli_auth_codes_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'consumed'::text, 'expired'::text])))
);


--
-- Name: compute_nodes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.compute_nodes (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    target_url text NOT NULL,
    vpcpus integer NOT NULL,
    mem_mb integer NOT NULL,
    max_concurrency integer NOT NULL,
    admission_ceiling_mb integer NOT NULL,
    active boolean DEFAULT true NOT NULL,
    last_heartbeat_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT compute_nodes_admission_ceiling_mb_check CHECK ((admission_ceiling_mb > 0)),
    CONSTRAINT compute_nodes_max_concurrency_check CHECK ((max_concurrency > 0)),
    CONSTRAINT compute_nodes_mem_mb_check CHECK ((mem_mb > 0)),
    CONSTRAINT compute_nodes_target_url_check CHECK ((target_url ~ '^(unix|tcp|dns)://'::text)),
    CONSTRAINT compute_nodes_vpcpus_check CHECK ((vpcpus > 0))
);


--
-- Name: crons; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.crons (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    app_id uuid NOT NULL,
    schedule text NOT NULL,
    path text DEFAULT '/'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_fired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: custom_domains; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.custom_domains (
    domain public.citext NOT NULL,
    app_id uuid NOT NULL,
    verified_at timestamp with time zone,
    challenge_token text DEFAULT ''::text NOT NULL,
    app_id_redirect uuid
);


--
-- Name: deployment_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployment_logs (
    deployment_id uuid NOT NULL,
    seq bigint NOT NULL,
    stream text DEFAULT 'stdout'::text NOT NULL,
    line text NOT NULL,
    written_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: deployment_logs_seq_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.deployment_logs_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: deployment_logs_seq_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.deployment_logs_seq_seq OWNED BY public.deployment_logs.seq;


--
-- Name: deployments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    app_id uuid NOT NULL,
    build_id uuid,
    image_digest text NOT NULL,
    rootfs_path text,
    rootfs_bytes bigint,
    status text NOT NULL,
    error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    kind text DEFAULT 'image'::text NOT NULL,
    source_path text,
    source_bytes bigint,
    handler text,
    log_path text,
    error_code text,
    rootfs_key text DEFAULT ''::text NOT NULL,
    CONSTRAINT deployments_kind_check CHECK ((kind = ANY (ARRAY['image'::text, 'tarball'::text, 'dockerfile'::text]))),
    CONSTRAINT deployments_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'building'::text, 'imaging'::text, 'snapshotting'::text, 'live'::text, 'failed'::text, 'superseded'::text])))
);


--
-- Name: events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.events (
    id bigint NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    actor text NOT NULL,
    kind text NOT NULL,
    subject uuid,
    data jsonb
);


--
-- Name: events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.events ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: gdpr_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.gdpr_requests (
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    account_email text NOT NULL,
    action text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT gdpr_requests_action_check CHECK ((action = ANY (ARRAY['export'::text, 'delete'::text, 'restore'::text])))
);


--
-- Name: goose_db_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.goose_db_version (
    id integer NOT NULL,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp without time zone DEFAULT now() NOT NULL
);


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.goose_db_version ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.goose_db_version_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: idempotency_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.idempotency_keys (
    key text NOT NULL,
    account_id uuid NOT NULL,
    response_status integer NOT NULL,
    response_body bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: instances; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.instances (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    app_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    state text NOT NULL,
    netns text,
    guest_uid integer,
    host_ip inet,
    ram_mb integer NOT NULL,
    started_at timestamp with time zone,
    last_request_at timestamp with time zone,
    parked_at timestamp with time zone,
    terminal_at timestamp with time zone,
    node_id uuid NOT NULL,
    wake_id uuid DEFAULT gen_random_uuid() NOT NULL,
    CONSTRAINT instances_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'cold_booting'::text, 'waking'::text, 'running'::text, 'parked'::text, 'stopped'::text, 'evicting_account_deleting'::text])))
);


--
-- Name: invocations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.invocations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    app_id uuid NOT NULL,
    account_id uuid NOT NULL,
    source text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    headers jsonb DEFAULT '{}'::jsonb NOT NULL,
    due_at timestamp with time zone DEFAULT now() NOT NULL,
    method text DEFAULT 'POST'::text NOT NULL,
    path text DEFAULT '/'::text NOT NULL,
    cron_id uuid,
    scheduled_at timestamp with time zone,
    ack_url text,
    result jsonb,
    lease_expires_at timestamp with time zone,
    received_at timestamp with time zone,
    completed_at timestamp with time zone,
    instance_id text,
    attempts integer DEFAULT 0 NOT NULL,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invocations_source_check CHECK ((source = ANY (ARRAY['async_invoke'::text, 'queue'::text, 'delayed_task'::text, 'cron'::text]))),
    CONSTRAINT invocations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'dispatching'::text, 'completed'::text, 'failed'::text, 'cancelled'::text])))
);


--
-- Name: invocations_pending_per_app; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.invocations_pending_per_app AS
 SELECT app_id,
    source,
    count(*) AS pending
   FROM public.invocations
  WHERE (state = ANY (ARRAY['pending'::text, 'dispatching'::text]))
  GROUP BY app_id, source;


--
-- Name: login_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.login_tokens (
    token_hash bytea NOT NULL,
    account_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone
);


--
-- Name: oauth_links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oauth_links (
    provider text NOT NULL,
    provider_subject text NOT NULL,
    account_id uuid NOT NULL,
    email text NOT NULL,
    email_verified boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: paddle_overage_dedupe; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.paddle_overage_dedupe (
    account_id uuid NOT NULL,
    month timestamp with time zone,
    pushed_at timestamp with time zone DEFAULT now() NOT NULL,
    window_start timestamp with time zone NOT NULL,
    state text DEFAULT 'completed'::text NOT NULL,
    claimed_at timestamp with time zone,
    claimed_by text,
    CONSTRAINT paddle_overage_dedupe_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'completed'::text])))
);


--
-- Name: recent_build_claims; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recent_build_claims (
    account_id uuid NOT NULL,
    claimed_at timestamp with time zone DEFAULT now() NOT NULL,
    build_id uuid NOT NULL
);


--
-- Name: snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.snapshots (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    deployment_id uuid NOT NULL,
    fc_version text NOT NULL,
    mem_bytes bigint NOT NULL,
    disk_bytes bigint NOT NULL,
    stale boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    storage_key text DEFAULT ''::text NOT NULL
);


--
-- Name: stripe_push_dedupe; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.stripe_push_dedupe (
    account_id uuid NOT NULL,
    hour timestamp with time zone NOT NULL,
    pushed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_minutes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_minutes (
    account_id uuid NOT NULL,
    app_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    minute timestamp with time zone NOT NULL,
    mb_seconds bigint NOT NULL,
    requests integer DEFAULT 0 NOT NULL
);


--
-- Name: usage_monthly; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.usage_monthly AS
 SELECT account_id,
    app_id,
    date_trunc('month'::text, minute) AS month,
    sum(mb_seconds) AS mb_seconds,
    sum(requests) AS requests
   FROM public.usage_minutes
  GROUP BY account_id, app_id, (date_trunc('month'::text, minute));


--
-- Name: deployment_logs seq; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_logs ALTER COLUMN seq SET DEFAULT nextval('public.deployment_logs_seq_seq'::regclass);


--
-- Name: account_passwords account_passwords_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_passwords
    ADD CONSTRAINT account_passwords_pkey PRIMARY KEY (account_id);


--
-- Name: accounts accounts_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_email_key UNIQUE (email);


--
-- Name: accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


--
-- Name: accounts accounts_stripe_customer_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_stripe_customer_id_key UNIQUE (provider_customer_id);


--
-- Name: api_keys api_keys_key_sha256_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_sha256_key UNIQUE (key_sha256);


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);


--
-- Name: app_secrets app_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_secrets
    ADD CONSTRAINT app_secrets_pkey PRIMARY KEY (app_id, key);


--
-- Name: apps apps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_pkey PRIMARY KEY (id);


--
-- Name: apps apps_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_slug_key UNIQUE (slug);


--
-- Name: builds builds_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builds
    ADD CONSTRAINT builds_pkey PRIMARY KEY (id);


--
-- Name: cli_auth_codes cli_auth_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cli_auth_codes
    ADD CONSTRAINT cli_auth_codes_pkey PRIMARY KEY (token_hash);


--
-- Name: compute_nodes compute_nodes_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.compute_nodes
    ADD CONSTRAINT compute_nodes_name_key UNIQUE (name);


--
-- Name: compute_nodes compute_nodes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.compute_nodes
    ADD CONSTRAINT compute_nodes_pkey PRIMARY KEY (id);


--
-- Name: crons crons_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.crons
    ADD CONSTRAINT crons_pkey PRIMARY KEY (id);


--
-- Name: custom_domains custom_domains_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.custom_domains
    ADD CONSTRAINT custom_domains_pkey PRIMARY KEY (domain);


--
-- Name: deployment_logs deployment_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_logs
    ADD CONSTRAINT deployment_logs_pkey PRIMARY KEY (deployment_id, seq);


--
-- Name: deployments deployments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_pkey PRIMARY KEY (id);


--
-- Name: events events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);


--
-- Name: gdpr_requests gdpr_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.gdpr_requests
    ADD CONSTRAINT gdpr_requests_pkey PRIMARY KEY (id);


--
-- Name: goose_db_version goose_db_version_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.goose_db_version
    ADD CONSTRAINT goose_db_version_pkey PRIMARY KEY (id);


--
-- Name: idempotency_keys idempotency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (account_id, key);


--
-- Name: instances instances_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.instances
    ADD CONSTRAINT instances_pkey PRIMARY KEY (id);


--
-- Name: invocations invocations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invocations
    ADD CONSTRAINT invocations_pkey PRIMARY KEY (id);


--
-- Name: login_tokens login_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.login_tokens
    ADD CONSTRAINT login_tokens_pkey PRIMARY KEY (token_hash);


--
-- Name: oauth_links oauth_links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oauth_links
    ADD CONSTRAINT oauth_links_pkey PRIMARY KEY (provider, provider_subject);


--
-- Name: paddle_overage_dedupe paddle_overage_dedupe_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.paddle_overage_dedupe
    ADD CONSTRAINT paddle_overage_dedupe_pkey PRIMARY KEY (account_id, window_start);


--
-- Name: snapshots snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.snapshots
    ADD CONSTRAINT snapshots_pkey PRIMARY KEY (id);


--
-- Name: stripe_push_dedupe stripe_push_dedupe_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_push_dedupe
    ADD CONSTRAINT stripe_push_dedupe_pkey PRIMARY KEY (account_id, hour);


--
-- Name: usage_minutes usage_minutes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_minutes
    ADD CONSTRAINT usage_minutes_pkey PRIMARY KEY (instance_id, minute);


--
-- Name: accounts_deletion_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX accounts_deletion_pending_idx ON public.accounts USING btree (deletion_requested_at) WHERE (status = 'deleted_pending'::text);


--
-- Name: accounts_past_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX accounts_past_due_idx ON public.accounts USING btree (past_due_at) WHERE (status = 'past_due'::text);


--
-- Name: api_keys_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_account_idx ON public.api_keys USING btree (account_id);


--
-- Name: app_secrets_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_secrets_account_idx ON public.app_secrets USING btree (account_id);


--
-- Name: app_secrets_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_secrets_app_idx ON public.app_secrets USING btree (app_id);


--
-- Name: apps_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_account_idx ON public.apps USING btree (account_id, status);


--
-- Name: apps_github_install_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_github_install_id_idx ON public.apps USING btree (github_install_id) WHERE (github_install_id IS NOT NULL);


--
-- Name: apps_github_install_repo_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX apps_github_install_repo_uniq ON public.apps USING btree (github_install_id, github_repo_full_name) WHERE ((github_install_id IS NOT NULL) AND (github_repo_full_name IS NOT NULL));


--
-- Name: builds_running_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX builds_running_started_idx ON public.builds USING btree (started_at) WHERE (status = 'running'::text);


--
-- Name: cli_auth_codes_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cli_auth_codes_pending_idx ON public.cli_auth_codes USING btree (status, expires_at) WHERE (status = 'pending'::text);


--
-- Name: compute_nodes_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX compute_nodes_active_idx ON public.compute_nodes USING btree (name) WHERE (active = true);


--
-- Name: crons_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX crons_app_idx ON public.crons USING btree (app_id) WHERE enabled;


--
-- Name: custom_domains_unverified_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX custom_domains_unverified_idx ON public.custom_domains USING btree (domain) WHERE (verified_at IS NULL);


--
-- Name: deployment_logs_seq_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployment_logs_seq_idx ON public.deployment_logs USING btree (deployment_id, seq DESC);


--
-- Name: deployments_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployments_app_idx ON public.deployments USING btree (app_id, created_at DESC);


--
-- Name: deployments_failed_error_code_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployments_failed_error_code_idx ON public.deployments USING btree (error_code) WHERE (status = 'failed'::text);


--
-- Name: events_subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX events_subject_idx ON public.events USING btree (subject, at DESC);


--
-- Name: gdpr_requests_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX gdpr_requests_account_idx ON public.gdpr_requests USING btree (account_id, requested_at DESC);


--
-- Name: instances_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX instances_app_idx ON public.instances USING btree (app_id, state);


--
-- Name: instances_reaper_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX instances_reaper_state_idx ON public.instances USING btree (started_at DESC) WHERE (state = ANY (ARRAY['running'::text, 'waking'::text, 'cold_booting'::text, 'snapshotting'::text]));


--
-- Name: instances_terminal_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX instances_terminal_at_idx ON public.instances USING btree (terminal_at) WHERE (state = ANY (ARRAY['stopped'::text, 'failed'::text]));


--
-- Name: instances_wake_id_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX instances_wake_id_app_idx ON public.instances USING btree (app_id, wake_id) WHERE (state = ANY (ARRAY['waking'::text, 'cold_booting'::text, 'running'::text, 'snapshotting'::text, 'parked'::text]));


--
-- Name: instances_watchdog_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX instances_watchdog_state_idx ON public.instances USING btree (state, started_at) WHERE (state = ANY (ARRAY['waking'::text, 'cold_booting'::text, 'snapshotting'::text]));


--
-- Name: invocations_app_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invocations_app_pending_idx ON public.invocations USING btree (app_id, source, state) WHERE (state = ANY (ARRAY['pending'::text, 'dispatching'::text]));


--
-- Name: invocations_delayed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invocations_delayed_idx ON public.invocations USING btree (app_id, scheduled_at) WHERE (source = 'delayed_task'::text);


--
-- Name: invocations_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invocations_due_idx ON public.invocations USING btree (due_at) WHERE (state = 'pending'::text);


--
-- Name: invocations_instance_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invocations_instance_idx ON public.invocations USING btree (instance_id, due_at) WHERE (state = 'dispatching'::text);


--
-- Name: login_tokens_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX login_tokens_account_idx ON public.login_tokens USING btree (account_id, expires_at);


--
-- Name: oauth_links_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX oauth_links_account_idx ON public.oauth_links USING btree (account_id);


--
-- Name: paddle_overage_dedupe_month_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX paddle_overage_dedupe_month_idx ON public.paddle_overage_dedupe USING btree (month);


--
-- Name: paddle_overage_dedupe_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX paddle_overage_dedupe_pending_idx ON public.paddle_overage_dedupe USING btree (claimed_at) WHERE (state = 'pending'::text);


--
-- Name: recent_build_claims_account_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recent_build_claims_account_id_idx ON public.recent_build_claims USING btree (account_id);


--
-- Name: recent_build_claims_claimed_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recent_build_claims_claimed_at_idx ON public.recent_build_claims USING btree (claimed_at);


--
-- Name: snapshots_deployment_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX snapshots_deployment_idx ON public.snapshots USING btree (deployment_id);


--
-- Name: stripe_push_dedupe_hour_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX stripe_push_dedupe_hour_idx ON public.stripe_push_dedupe USING btree (hour);


--
-- Name: apps apps_egress_allowlist_cidr; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER apps_egress_allowlist_cidr BEFORE INSERT OR UPDATE OF egress_allowlist ON public.apps FOR EACH ROW EXECUTE FUNCTION public.apps_egress_allowlist_cidr_check();


--
-- Name: compute_nodes compute_node_changed_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER compute_node_changed_trg AFTER INSERT OR UPDATE ON public.compute_nodes FOR EACH ROW EXECUTE FUNCTION public.compute_node_notify();


--
-- Name: instances instances_started_at_set_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER instances_started_at_set_trg BEFORE INSERT ON public.instances FOR EACH ROW EXECUTE FUNCTION public.instances_started_at_set();


--
-- Name: invocations invocation_done_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER invocation_done_trg AFTER UPDATE ON public.invocations FOR EACH ROW EXECUTE FUNCTION public.invocation_done_notify();


--
-- Name: invocations invocation_due_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER invocation_due_trg AFTER INSERT OR UPDATE OF state ON public.invocations FOR EACH ROW WHEN ((new.state = 'pending'::text)) EXECUTE FUNCTION public.invocation_due_notify();


--
-- Name: account_passwords account_passwords_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_passwords
    ADD CONSTRAINT account_passwords_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: api_keys api_keys_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: app_secrets app_secrets_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_secrets
    ADD CONSTRAINT app_secrets_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: apps apps_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: builds builds_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builds
    ADD CONSTRAINT builds_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id);


--
-- Name: cli_auth_codes cli_auth_codes_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cli_auth_codes
    ADD CONSTRAINT cli_auth_codes_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: crons crons_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.crons
    ADD CONSTRAINT crons_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id);


--
-- Name: custom_domains custom_domains_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.custom_domains
    ADD CONSTRAINT custom_domains_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id);


--
-- Name: custom_domains custom_domains_app_id_redirect_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.custom_domains
    ADD CONSTRAINT custom_domains_app_id_redirect_fkey FOREIGN KEY (app_id_redirect) REFERENCES public.apps(id);


--
-- Name: deployment_logs deployment_logs_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_logs
    ADD CONSTRAINT deployment_logs_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE CASCADE;


--
-- Name: deployments deployments_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id);


--
-- Name: idempotency_keys idempotency_keys_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: instances instances_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.instances
    ADD CONSTRAINT instances_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id);


--
-- Name: instances instances_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.instances
    ADD CONSTRAINT instances_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id);


--
-- Name: instances instances_node_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.instances
    ADD CONSTRAINT instances_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.compute_nodes(id);


--
-- Name: invocations invocations_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invocations
    ADD CONSTRAINT invocations_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: invocations invocations_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invocations
    ADD CONSTRAINT invocations_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id);


--
-- Name: invocations invocations_cron_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invocations
    ADD CONSTRAINT invocations_cron_id_fkey FOREIGN KEY (cron_id) REFERENCES public.crons(id);


--
-- Name: login_tokens login_tokens_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.login_tokens
    ADD CONSTRAINT login_tokens_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: oauth_links oauth_links_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oauth_links
    ADD CONSTRAINT oauth_links_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: paddle_overage_dedupe paddle_overage_dedupe_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.paddle_overage_dedupe
    ADD CONSTRAINT paddle_overage_dedupe_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: recent_build_claims recent_build_claims_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recent_build_claims
    ADD CONSTRAINT recent_build_claims_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
-- Name: snapshots snapshots_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.snapshots
    ADD CONSTRAINT snapshots_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id);


--
-- Name: stripe_push_dedupe stripe_push_dedupe_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_push_dedupe
    ADD CONSTRAINT stripe_push_dedupe_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;


--
--


