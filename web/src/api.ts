import { GraylingClient } from '@saker/grayling-client';

// The console-wide client singleton. SessionProvider injects onUnauthorized.
export const client = new GraylingClient();
