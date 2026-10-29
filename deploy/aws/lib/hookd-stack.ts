import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { Aws, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib';
import * as cloudfront from 'aws-cdk-lib/aws-cloudfront';
import * as origins from 'aws-cdk-lib/aws-cloudfront-origins';
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
// Written out, not a token, so cdk-nag can see the instance only accepts traffic from inside the VPC.
const vpcCidr = '10.0.0.0/16';

// A single test box: one EC2 instance running hookd serve on SQLite, with HTTPS from CloudFront.
export class HookdStack extends Stack {
  constructor(scope: Construct, id: string, props: StackProps) {
    super(scope, id, props);

    // Network: one AZ; the instance is private and reaches webhook endpoints through one NAT gateway.
    const network = new Construct(this, 'Network');
    component(network, 'hookd serve');
    purpose(network, 'Private subnet for the instance, with a NAT gateway for outbound deliveries');
    const vpc = new ec2.Vpc(network, 'Vpc', {
      ipAddresses: ec2.IpAddresses.cidr(vpcCidr),
      availabilityZones: [`${this.region}a`],
      natGateways: 1,
      subnetConfiguration: [
        { name: 'public', subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
        { name: 'private', subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS, cidrMask: 24 },
      ],
    });
    vpc.addFlowLog('FlowLog', {
      destination: ec2.FlowLogDestination.toCloudWatchLogs(
        new logs.LogGroup(network, 'FlowLogs', { retention: logs.RetentionDays.ONE_WEEK }),
      ),
    });

    // hookd serve: one systemd service on one instance; SQLite lives on its encrypted root volume.
    // The source goes up as a zip (no Docker needed to deploy) and the box builds the binary at boot.
    const app = new Construct(this, 'Serve');
    component(app, 'hookd serve');
    purpose(app, 'The hookd serve process: HTTP API on 8080 and delivery workers');

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
      description: 'hookd instance: 8080 from CloudFront VPC origin inside the VPC; all outbound for deliveries',
      allowAllOutbound: true,
    });
    sg.addIngressRule(ec2.Peer.ipv4(vpcCidr), ec2.Port.tcp(8080), 'CloudFront VPC origin');

    const userData = ec2.UserData.forLinux();
    const instance = new ec2.Instance(app, 'Box', {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
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
      'dnf install -y golang unzip amazon-cloudwatch-agent',
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
      'cat > /etc/hookd/hookd.env <<EOF',
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
      // Ship hookd's log file to CloudWatch, one stream per instance.
      "cat > /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json <<'EOF'",
      `{"logs":{"logs_collected":{"files":{"collect_list":[{"file_path":"/var/log/hookd/hookd.log","log_group_name":"${logGroup.logGroupName}","log_stream_name":"hookd-{instance_id}"}]}}}}`,
      'EOF',
      '/opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl -a fetch-config -m ec2 -s -c file:/opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json',
      'systemctl daemon-reload',
      'systemctl enable --now hookd',
      'for i in $(seq 1 60); do curl -sf http://127.0.0.1:8080/healthz && break; sleep 5; done',
      'curl -sf http://127.0.0.1:8080/healthz',
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

    // Load balancer or reverse proxy: CloudFront terminates TLS and reaches the box privately.
    const edge = new Construct(this, 'Edge');
    component(edge, 'Load balancer or reverse proxy');
    purpose(edge, 'HTTPS for the API at CloudFront, forwarded privately to the box on port 8080');

    // Never cache across API keys: Authorization is part of the key, and nothing is kept past 1 s.
    const cachePolicy = new cloudfront.CachePolicy(edge, 'ApiCache', {
      comment: 'hookd API: effectively no caching, keyed by Authorization',
      minTtl: Duration.seconds(0),
      defaultTtl: Duration.seconds(0),
      maxTtl: Duration.seconds(1),
      headerBehavior: cloudfront.CacheHeaderBehavior.allowList('Authorization'),
      queryStringBehavior: cloudfront.CacheQueryStringBehavior.all(),
      cookieBehavior: cloudfront.CacheCookieBehavior.none(),
    });
    const cdn = new cloudfront.Distribution(edge, 'Cdn', {
      comment: 'hookd API',
      priceClass: cloudfront.PriceClass.PRICE_CLASS_100,
      defaultBehavior: {
        origin: origins.VpcOrigin.withEc2Instance(instance, {
          protocolPolicy: cloudfront.OriginProtocolPolicy.HTTP_ONLY,
          httpPort: 8080,
        }),
        viewerProtocolPolicy: cloudfront.ViewerProtocolPolicy.HTTPS_ONLY,
        allowedMethods: cloudfront.AllowedMethods.ALLOW_ALL,
        cachePolicy,
        originRequestPolicy: cloudfront.OriginRequestPolicy.ALL_VIEWER_EXCEPT_HOST_HEADER,
      },
    });

    address(this, `https://${cdn.distributionDomainName}`, { healthPath: '/healthz' });

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
    NagSuppressions.addResourceSuppressions(instance, [
      { id: 'AwsSolutions-EC28', reason: 'A test box; basic 5-minute monitoring is enough.' },
      { id: 'AwsSolutions-EC29', reason: 'A test box that is meant to be replaced and deleted freely.' },
    ]);
    NagSuppressions.addResourceSuppressions(cdn, [
      { id: 'AwsSolutions-CFR1', reason: 'The API may be called from any country.' },
      { id: 'AwsSolutions-CFR2', reason: 'A test box with no WAF; every API call needs a bearer API key.' },
      { id: 'AwsSolutions-CFR3', reason: 'hookd logs every API request to CloudWatch.' },
      { id: 'AwsSolutions-CFR4', reason: 'Uses the default cloudfront.net certificate; there is no custom domain.' },
    ]);
  }
}
