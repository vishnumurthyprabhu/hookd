import { join } from 'node:path';
import { Aws, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib';
import * as cloudfront from 'aws-cdk-lib/aws-cloudfront';
import * as origins from 'aws-cdk-lib/aws-cloudfront-origins';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecrAssets from 'aws-cdk-lib/aws-ecr-assets';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as logs from 'aws-cdk-lib/aws-logs';
import { NagSuppressions } from 'cdk-nag';
import { Construct } from 'constructs';
import { address, component, purpose } from './beadsurf';

const repoRoot = join(__dirname, '..', '..', '..');
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

    // hookd serve: one container on one instance; SQLite lives on its encrypted root volume.
    const app = new Construct(this, 'Serve');
    component(app, 'hookd serve');
    purpose(app, 'The hookd serve process: HTTP API on 8080 and delivery workers');

    const image = new ecrAssets.DockerImageAsset(app, 'Image', {
      directory: repoRoot,
      file: 'deploy/aws/lib/Dockerfile',
      platform: ecrAssets.Platform.LINUX_AMD64,
      exclude: ['.git', 'node_modules', '.surface', 'deploy/aws/cdk.out*'],
    });

    const logGroup = new logs.LogGroup(app, 'Logs', {
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: RemovalPolicy.DESTROY,
    });

    const role = new iam.Role(app, 'InstanceRole', {
      assumedBy: new iam.ServicePrincipal('ec2.amazonaws.com'),
      description: 'hookd instance: pull its image, write its logs, signal CloudFormation',
    });
    image.repository.grantPull(role);
    logGroup.grantWrite(role);
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
      // A new image replaces the box, so the new user data runs.
      userDataCausesReplacement: true,
      resourceSignalTimeout: Duration.minutes(15),
      // IMDSv2 with hop limit 1: the container cannot reach instance metadata, even through a
      // webhook endpoint URL that points at 169.254.169.254.
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

    const registry = `${this.account}.dkr.ecr.${this.region}.${this.urlSuffix}`;
    userData.addCommands(
      'set -euo pipefail',
      'TOKEN=$(curl -sfX PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 300")',
      'INSTANCE_ID=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)',
      `signal() { aws cloudformation signal-resource --region ${this.region} --stack-name ${this.stackName} --logical-resource-id ${instanceLogicalId} --unique-id "$INSTANCE_ID" --status "$1" || true; }`,
      "trap 'signal FAILURE' ERR",
      'mkdir -p /data',
      'chown 65532:65532 /data',
      'dnf install -y docker',
      'systemctl enable --now docker',
      `aws ecr get-login-password --region ${this.region} | docker login --username AWS --password-stdin ${registry}`,
      `docker pull ${image.imageUri}`,
      'mkdir -p /etc/hookd',
      'cat > /etc/hookd/hookd.env <<EOF',
      'HOOKD_DB_DRIVER=sqlite',
      'HOOKD_DB_PATH=/data/hookd.db',
      'HOOKD_AUTO_MIGRATE=true',
      'HOOKD_LOG_FORMAT=json',
      'EOF',
      'chmod 600 /etc/hookd/hookd.env',
      'cat > /etc/systemd/system/hookd.service <<EOF',
      '[Unit]',
      'Description=Hookd webhook delivery',
      'After=docker.service',
      'Requires=docker.service',
      '[Service]',
      'Restart=always',
      'RestartSec=2',
      'ExecStartPre=-/usr/bin/docker rm -f hookd',
      `ExecStart=/usr/bin/docker run --name hookd -p 8080:8080 -v /data:/data --env-file /etc/hookd/hookd.env --log-driver awslogs --log-opt awslogs-region=${this.region} --log-opt awslogs-group=${logGroup.logGroupName} --log-opt awslogs-stream=hookd-$INSTANCE_ID ${image.imageUri}`,
      'ExecStop=/usr/bin/docker stop -t 30 hookd',
      'TimeoutStopSec=45',
      '[Install]',
      'WantedBy=multi-user.target',
      'EOF',
      'systemctl daemon-reload',
      'systemctl enable --now hookd',
      'for i in $(seq 1 60); do curl -sf http://127.0.0.1:8080/healthz && break; sleep 5; done',
      'curl -sf http://127.0.0.1:8080/healthz',
      'signal SUCCESS',
    );

    // Operator commands: a shell on the box through Session Manager, then
    // `sudo docker exec hookd hookd account create --name NAME` and the like.
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
      [{ id: 'AwsSolutions-IAM5', reason: 'ecr:GetAuthorizationToken does not support resource-level permissions.' }],
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
