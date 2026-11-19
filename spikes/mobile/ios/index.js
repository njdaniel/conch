/**
 * @format
 */

import { AppRegistry } from 'react-native';
import { registerGlobals } from '@livekit/react-native';
import App from './App';
import { name as appName } from './app.json';

// LiveKit's globals (WebRTC, URL, streams) before anything uses livekit-client.
registerGlobals();

AppRegistry.registerComponent(appName, () => App);
