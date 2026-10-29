import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { Aws, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as s3assets from 'aws-cdk-lib/aws-s3-assets';
import { NagSuppressions } from 'cdk-nag';
import { Construct } from 'constructs';
import { address, component, purpose } from './beadsurf';

// The hookd repository: the nearest folder above this app with hookd's go.mod. The app itself may sit
// at a different depth (deploy/aws, or a working copy under .surface), so the depth is not fixed.
function findRepoRoot(): string {
  for (let dir = __dirname; ; dir = dirname(dir)) {
    if (existsSync(join(dir, 'go.mod')) && existsSync(join(dir, 'cmd', 'hookd'))) return dir;
    if (dirname(dir) === dir) throw new Error(`No hookd repository (go.mod and cmd/hookd) above ${__dirname}`);
  }
}
const repoRoot = findRepoRoot();

// nginx on the box: HTTPS on 443 with a self-signed certificate, proxied to hookd on 127.0.0.1:8080.
// Port 80 answers only /healthz (for the health check) and redirects everything else to HTTPS.
const nginxConf = `user nginx;
worker_processes auto;
error_log /var/log/nginx/error.log;
pid /run/nginx.pid;
events { worker_connections 1024; }
http {
  access_log /var/log/nginx/access.log;
  server_tokens off;
  # Above HOOKD_MAX_PAYLOAD_BYTES (256 KB), so hookd rather than nginx answers oversized events.
  client_max_body_size 1m;
  server {
    listen 80 default_server;
    location = /healthz { proxy_pass http://127.0.0.1:8080; }
    location / { return 301 https://$host$request_uri; }
  }
  server {
    listen 443 ssl default_server;
    ssl_certificate     /etc/nginx/tls/hookd.crt;
    ssl_certificate_key /etc/nginx/tls/hookd.key;
    # The admin UI has no sign-in: never through nginx. Reach it on the box's loopback through a
    # Session Manager port forward instead. / only redirects to /admin/.
    location /admin { return 404; }
    location = / { return 404; }
    location / {
      proxy_pass http://127.0.0.1:8080;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;
      proxy_set_header X-Forwarded-Proto https;
    }
  }
}`;

// A single test box: one EC2 instance in a public subnet running nginx and hookd serve on SQLite.
export class HookdStack extends Stack {
  constructor(scope: Construct, id: string, props: StackProps) {
    super(scope, id, props);

    // Network: one AZ, one public subnet, no NAT gateway; the box reaches webhook endpoints directly.
    const network = new Construct(this, 'Network');
    component(network, 'hookd serve');
    purpose(network, 'One public subnet for the box');
    const vpc = new ec2.Vpc(network, 'Vpc', {
      ipAddresses: ec2.IpAddresses.cidr('10.0.0.0/16'),
      availabilityZones: [`${this.region}a`],
      natGateways: 0,
      subnetConfiguration: [{ name: 'public', subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 }],
    });
    vpc.addFlowLog('FlowLog', {
      destination: ec2.FlowLogDestination.toCloudWatchLogs(
        new logs.LogGroup(network, 'FlowLogs', {
          retention: logs.RetentionDays.ONE_WEEK,
          removalPolicy: RemovalPolicy.DESTROY,
        }),
      ),
    });

    // hookd serve: one systemd service on one instance; SQLite lives on its encrypted root volume.
    // The source goes up as a zip (no Docker needed to deploy) and the box builds the binary at boot.
    const app = new Construct(this, 'Serve');
    component(app, 'hookd serve');
    purpose(app, 'The box: nginx on 443 in front of hookd serve (HTTP API and delivery workers)');

    const source = new s3assets.Asset(app, 'Source', {
      path: repoRoot,
      // cdk.out anywhere: the asset is staged into it, so including it would copy the asset into itself.
      exclude: ['.git', 'node_modules', '.surface', '.beads', 'cdk.out*', '**/cdk.out*', '*.db', '*.db-wal', '*.db-shm'],
    });

    const logGroup = new logs.LogGroup(app, 'Logs', {
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: RemovalPolicy.DESTROY,
    });

    const role = new iam.Role(app, 'InstanceRole', {
      assumedBy: new iam.ServicePrincipal('ec2.amazonaws.com'),
      description: 'hookd instance: read its source, write its logs, signal CloudFormation',
    });
    source.grantRead(role);
    logGroup.grantWrite(role);
    role.addToPolicy(new iam.PolicyStatement({ actions: ['logs:DescribeLogStreams'], resources: [logGroup.logGroupArn] }));
    role.addToPolicy(
      new iam.PolicyStatement({ actions: ['cloudformation:SignalResource'], resources: [Aws.STACK_ID] }),
    );

    const sg = new ec2.SecurityGroup(app, 'InstanceSg', {
      vpc,
      description: 'hookd box: HTTPS 443 and HTTP 80 (health and redirect) from anywhere; all outbound for deliveries',
      allowAllOutbound: true,
    });
    sg.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(443), 'hookd API over HTTPS');
    sg.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(80), 'Health check and redirect to HTTPS');

    const userData = ec2.UserData.forLinux();
    const instance = new ec2.Instance(app, 'Box', {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
      associatePublicIpAddress: true,
      machineImage: ec2.MachineImage.latestAmazonLinux2023(),
      instanceType: ec2.InstanceType.of(ec2.InstanceClass.T3, ec2.InstanceSize.SMALL),
      role,
      securityGroup: sg,
      userData,
      // New source replaces the box, so the new user data runs.
      userDataCausesReplacement: true,
      // The first boot downloads the Go toolchain and modules and builds hookd.
      resourceSignalTimeout: Duration.minutes(25),
      // IMDSv2 only; the service also has IPAddressDeny for 169.254.169.254, so a webhook
      // endpoint URL that points at instance metadata gets nothing.
      httpEndpoint: true,
      httpTokens: ec2.HttpTokens.REQUIRED,
      httpPutResponseHopLimit: 1,
      blockDevices: [
        {
          deviceName: '/dev/xvda',
          volume: ec2.BlockDeviceVolume.ebs(20, { encrypted: true, volumeType: ec2.EbsDeviceVolumeType.GP3 }),
        },
      ],
    });
    const instanceLogicalId = (instance.node.defaultChild as ec2.CfnInstance).logicalId;

    userData.addCommands(
      'set -euo pipefail',
      'TOKEN=$(curl -sfX PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 300")',
      'INSTANCE_ID=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)',
      `signal() { aws cloudformation signal-resource --region ${this.region} --stack-name ${this.stackName} --logical-resource-id ${instanceLogicalId} --unique-id "$INSTANCE_ID" --status "$1" || true; }`,
      "trap 'signal FAILURE' ERR",
      'dnf install -y golang unzip nginx openssl amazon-cloudwatch-agent',
      // Build hookd from the source zip. The distro Go fetches the Go 1.26 toolchain go.mod asks for,
      // checked against the Go checksum database.
      `aws s3 cp --region ${this.region} ${source.s3ObjectUrl} /tmp/hookd-src.zip`,
      'rm -rf /opt/hookd-src && mkdir -p /opt/hookd-src',
      'unzip -q /tmp/hookd-src.zip -d /opt/hookd-src',
      'export HOME=/root GOPATH=/root/go GOCACHE=/root/.cache/go-build GOTOOLCHAIN=go1.26.0+auto GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org CGO_ENABLED=0',
      'cd /opt/hookd-src',
      'go build -trimpath -ldflags="-s -w" -o /usr/local/bin/hookd ./cmd/hookd',
      'cd /',
      'useradd --system --no-create-home --shell /sbin/nologin hookd || true',
      'mkdir -p /data /var/log/hookd',
      'chown hookd:hookd /data',
      'mkdir -p /etc/hookd',
      // hookd listens on loopback only; nginx is the only way in.
      'cat > /etc/hookd/hookd.env <<EOF',
      'HOOKD_LISTEN_ADDR=127.0.0.1:8080',
      'HOOKD_DB_DRIVER=sqlite',
      'HOOKD_DB_PATH=/data/hookd.db',
      'HOOKD_AUTO_MIGRATE=true',
      'HOOKD_LOG_FORMAT=json',
      'EOF',
      'chown root:hookd /etc/hookd/hookd.env',
      'chmod 640 /etc/hookd/hookd.env',
      'cat > /etc/systemd/system/hookd.service <<EOF',
      '[Unit]',
      'Description=Hookd webhook delivery',
      'After=network-online.target',
      'Wants=network-online.target',
      '[Service]',
      'User=hookd',
      'Group=hookd',
      'EnvironmentFile=/etc/hookd/hookd.env',
      'ExecStart=/usr/local/bin/hookd serve',
      'StandardOutput=append:/var/log/hookd/hookd.log',
      'StandardError=append:/var/log/hookd/hookd.log',
      'Restart=always',
      'RestartSec=2',
      'KillSignal=SIGTERM',
      'TimeoutStopSec=30',
      'NoNewPrivileges=yes',
      'IPAddressDeny=169.254.169.254',
      '[Install]',
      'WantedBy=multi-user.target',
      'EOF',
      // A self-signed certificate for nginx, made on the box; clients call the API with curl -k.
      'mkdir -p /etc/nginx/tls',
      'openssl req -x509 -newkey rsa:2048 -nodes -days 825 -subj "/CN=hookd" -keyout /etc/nginx/tls/hookd.key -out /etc/nginx/tls/hookd.crt',
      'chmod 600 /etc/nginx/tls/hookd.key',
      "cat > /etc/nginx/nginx.conf <<'EOF'",
      nginxConf,
      'EOF',
      'nginx -t',
      // Ship hookd's log file to CloudWatch, one stream per instance.
      "cat > /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json <<'EOF'",
      `{"logs":{"logs_collected":{"files":{"collect_list":[{"file_path":"/var/log/hookd/hookd.log","log_group_name":"${logGroup.logGroupName}","log_stream_name":"hookd-{instance_id}"}]}}}}`,
      'EOF',
      '/opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl -a fetch-config -m ec2 -s -c file:/opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json',
      'systemctl daemon-reload',
      'systemctl enable --now hookd',
      'systemctl enable --now nginx',
      'for i in $(seq 1 60); do curl -sf http://127.0.0.1:8080/healthz && break; sleep 5; done',
      'curl -sf http://127.0.0.1:8080/healthz',
      'curl -sfk https://127.0.0.1/healthz',
      'signal SUCCESS',
    );

    // Operator commands: a shell on the box through Session Manager, then
    // `sudo -u hookd bash -c 'set -a; . /etc/hookd/hookd.env; hookd account create --name NAME'` and the like.
    const operator = new Construct(this, 'Operator');
    component(operator, 'hookd operator commands');
    purpose(operator, 'Session Manager access to the box, to run hookd account and apikey commands');
    const ssmPolicy = new iam.Policy(operator, 'SessionManager', {
      statements: [
        new iam.PolicyStatement({
          actions: [
            'ssm:UpdateInstanceInformation',
            'ssmmessages:CreateControlChannel',
            'ssmmessages:CreateDataChannel',
            'ssmmessages:OpenControlChannel',
            'ssmmessages:OpenDataChannel',
          ],
          resources: ['*'],
        }),
      ],
    });
    role.attachInlinePolicy(ssmPolicy);

    // Load balancer or reverse proxy: nginx on the box itself; an Elastic IP keeps its address
    // when the box is replaced.
    const edge = new Construct(this, 'Edge');
    component(edge, 'Load balancer or reverse proxy');
    purpose(edge, 'Fixed public address of the box, where nginx terminates TLS');
    const eip = new ec2.CfnEIP(edge, 'Address', { domain: 'vpc', instanceId: instance.instanceId });

    // The health check goes to port 80, which serves only /healthz: the certificate on 443 is self-signed.
    address(this, `http://${eip.attrPublicIp}`, { healthPath: '/healthz' });

    // cdk-nag findings that are accepted, each with its reason.
    NagSuppressions.addResourceSuppressions(
      role,
      [
        {
          id: 'AwsSolutions-IAM5',
          reason: 'CDK asset read grants use s3:GetObject*/List* on the asset bucket; the source zip key is content-hashed.',
        },
      ],
      true,
    );
    NagSuppressions.addResourceSuppressions(ssmPolicy, [
      { id: 'AwsSolutions-IAM5', reason: 'Session Manager agent actions do not support resource-level permissions.' },
    ]);
    NagSuppressions.addResourceSuppressions(sg, [
      { id: 'AwsSolutions-EC23', reason: 'A public test API: 443 for the API and 80 for health and redirect only.' },
    ]);
    NagSuppressions.addResourceSuppressions(instance, [
      { id: 'AwsSolutions-EC28', reason: 'A test box; basic 5-minute monitoring is enough.' },
      { id: 'AwsSolutions-EC29', reason: 'A test box that is meant to be replaced and deleted freely.' },
    ]);
  }
}
