// Written by Software Factory, which writes it back whenever it is changed. Edit bin/ and the rest of lib/ instead.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { App, Aspects, CfnOutput, CfnResource, type IAspect, Stack, type StackProps, Tags } from 'aws-cdk-lib';
import { AwsSolutionsChecks, NagReportFormat } from 'cdk-nag';
import type { IConstruct } from 'constructs';

interface Marks {
  component?: string;
  purpose?: string;
}
const marks = new WeakMap<IConstruct, Marks>();

function mark(scope: IConstruct, added: Marks): void {
  marks.set(scope, { ...marks.get(scope), ...added });
}

// The profile component a construct serves; every resource below it is marked with it.
export function component(scope: IConstruct, name: string): void {
  mark(scope, { component: name });
}

// What a construct is for, in a few words; every resource below it is marked with it.
export function purpose(scope: IConstruct, text: string): void {
  mark(scope, { purpose: text });
}

const addressed = new WeakSet<IConstruct>();

// The service's public URL, which Bead Surf checks the health of after a deploy at the URL joined with healthPath.
// Call once in the app, and not at all when the project has no public address.
export function address(scope: IConstruct, url: string, { healthPath = '/' }: { healthPath?: string } = {}): void {
  const app = scope.node.root;
  if (addressed.has(app)) throw new Error('The app already has an address.');
  addressed.add(app);
  const stack = Stack.of(scope);
  new CfnOutput(stack, 'BeadSurfAddress', { value: url });
  new CfnOutput(stack, 'BeadSurfHealthPath', { value: healthPath });
}

// Copies the nearest marks above each resource into its template metadata, where Bead Surf reads them.
class CopyMarks implements IAspect {
  visit(node: IConstruct): void {
    if (!(node instanceof CfnResource)) return;
    let component: string | undefined;
    let purpose: string | undefined;
    for (const scope of [...node.node.scopes].reverse()) {
      const found = marks.get(scope);
      component ??= found?.component;
      purpose ??= found?.purpose;
    }
    if (component !== undefined) node.addMetadata('BeadSurf::Component', component);
    if (purpose !== undefined) node.addMetadata('BeadSurf::Purpose', purpose);
  }
}

function stackName(): string {
  const cdk = JSON.parse(readFileSync(join(process.cwd(), 'cdk.json'), 'utf8'));
  return cdk.context['beadsurf:stack'];
}

// Call once, right after new App().
export function beadsurf(app: App): void {
  Tags.of(app).add('beadsurf:project', stackName());
  const deployment = app.node.tryGetContext('beadsurf:deployment');
  if (deployment !== undefined) Tags.of(app).add('beadsurf:deployment', String(deployment));
  Aspects.of(app).add(new CopyMarks());
  const nag = app.node.tryGetContext('beadsurf:nag');
  if (nag === true || nag === 'true') Aspects.of(app).add(new AwsSolutionsChecks({ reports: true, reportFormats: [NagReportFormat.JSON] }));
}

// The stack's name, and the account and region the CDK CLI takes from the profile. No account or region is written in the code.
export function stackProps(): StackProps & { stackName: string } {
  return {
    stackName: stackName(),
    env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: process.env.CDK_DEFAULT_REGION },
  };
}
