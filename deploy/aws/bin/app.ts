import { App } from 'aws-cdk-lib';
import { beadsurf, stackProps } from '../lib/beadsurf';
import { HookdStack } from '../lib/hookd-stack';

const app = new App();
beadsurf(app);
const props = stackProps();
new HookdStack(app, props.stackName, props);
