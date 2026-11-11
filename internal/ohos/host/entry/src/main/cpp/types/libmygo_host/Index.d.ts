import { NodeContent, UIContext } from '@kit.ArkUI';

export interface HostWinConfig {
  winId: number;
  parentOrMask: number;
  x: number;
  y: number;
  w: number;
  h: number;
  minW: number;
  minH: number;
  maxW: number;
  maxH: number;
  flags: number;
  titleBarHeight: number;
  bgRGBA: number;
  title: string;
}

export interface HostMenuItem {
  id: number;
  kind: number;
  enabled: boolean;
  checked: boolean;
  label: string;
  accel: string;
  items: HostMenuItem[];
}

export interface HostDisplayInfo {
  id: number;
  label: string;
  x: number;
  y: number;
  w: number;
  h: number;
  wx: number;
  wy: number;
  ww: number;
  wh: number;
  scale: number;
  rotation: number;
  primary: boolean;
  internal: boolean;
}

export interface HostBridgeCallbacks {
  onWinCreate: (cfg: HostWinConfig) => void;
  onWinConfig: (cfg: HostWinConfig) => void;
  onWinAction: (winId: number, action: number) => void;
  onPopupMenu: (reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]) => void;
  onEngineQuit: () => void;
  onEngineRestart: () => void;
}

export interface HostModule {
  setBridgeCallbacks(cb: HostBridgeCallbacks): void;
  updateEnvInfo(
    pkgName: string,
    pkgVer: string,
    pkgId: string,
    locale: string,
    firstWeekday: number,
    hourCycle: number,
    langs: string[],
    displays: HostDisplayInfo[]
  ): void;
  bindWindow(
    winId: number,
    uiContext: UIContext,
    content: NodeContent,
    arkWindowId: number,
    scale: number,
    refreshRate: number,
    darkMode: boolean
  ): void;
  unbindWindow(winId: number): void;
  notifyWindowEvent(winId: number, op: number): void;
  notifyWindowRect(winId: number, xDip: number, yDip: number, wDip: number, hDip: number): void;
  notifyTitleBar(winId: number, heightDip: number, leftDip: number, rightDip: number): void;
  resolvePopupMenu(reqId: number, chosenId: number): void;
  setDarkMode(dark: boolean): void;
  crash(): void;
}

declare const host: HostModule;
export default host;
