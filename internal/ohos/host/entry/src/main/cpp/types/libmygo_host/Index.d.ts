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

export interface HostClipWrite {
  clear: boolean;
  concealed: boolean;
  text: string;
  html: string;
  uriList: string;
  png: ArrayBuffer;
}

export interface HostClipRep {
  format: string;
  data: ArrayBuffer;
}

export interface HostFileFilter {
  name: string;
  extensions: string[];
}

export interface HostFileDialog {
  reqId: number;
  winId: number;
  flags: number;
  title: string;
  defaultPath: string;
  buttonLabel: string;
  filters: HostFileFilter[];
}

export interface HostMessageBox {
  reqId: number;
  winId: number;
  defaultId: number;
  cancelId: number;
  checkboxChecked: boolean;
  boxType: string;
  title: string;
  message: string;
  detail: string;
  checkboxLabel: string;
  buttons: string[];
  destructiveIds: number[];
}

export interface HostNowPlaying {
  active: boolean;
  state: number;
  metaChanged: boolean;
  commands: number;
  durationMs: number;
  positionMs: number;
  rate: number;
  title: string;
  artist: string;
  album: string;
  artwork: ArrayBuffer;
}

export interface HostNotifShow {
  reqId: number;
  silent: boolean;
  id: string;
  title: string;
  subtitle: string;
  body: string;
  group: string;
  image: ArrayBuffer;
}

export interface HostPowerReq {
  keepSystem: boolean;
  keepDisplay: boolean;
  keepRunning: boolean;
  reason: string;
}

export interface HostCookieReq {
  reqId: number;
  op: number;
  incognito: boolean;
  siteDataTypes: number;
  url: string;
  headerOrName: string;
  domain: string;
  path: string;
}

export interface HostCookie {
  name: string;
  value: string;
  domain: string;
  path: string;
  expiresUnix: number;
  secure: boolean;
  httpOnly: boolean;
  sameSite: string;
}

export interface HostUserScript {
  source: string;
  atDocEnd: boolean;
  allFrames: boolean;
}

export interface HostWebSetup {
  webId: number;
  hostWinId: number;
  flags: number;
  bgRGBA: number;
  zoom: number;
  ua: string;
  schemes: string[];
  scripts: HostUserScript[];
}

export interface HostPlacedWebView {
  webId: number;
  frameX: number;
  frameY: number;
  frameW: number;
  frameH: number;
  clipX: number;
  clipY: number;
  clipW: number;
  clipH: number;
  pageW: number;
  pageH: number;
  r0: number;
  r1: number;
  r2: number;
  r3: number;
}

export interface HostSurfaceRegion {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface HostPlaceWebViews {
  winId: number;
  views: HostPlacedWebView[];
  surfaceRegions: HostSurfaceRegion[];
}

export interface HostWebAsyncReq {
  reqId: number;
  webId: number;
  op: number;
  landscape: boolean;
  background: boolean;
  pageWidth: number;
  pageHeight: number;
  marginTop: number;
  marginRight: number;
  marginBottom: number;
  marginLeft: number;
  body: string;
}

export interface HostSchemeHeader {
  name: string;
  headerValue: string;
}

export interface HostSchemeRes {
  reqId: number;
  op: number;
  status: number;
  headers: HostSchemeHeader[];
  chunk: ArrayBuffer;
  err: string;
}

export interface HostTrayUpdate {
  trayId: number;
  op: number;
  flags: number;
  id: string;
  title: string;
  tooltip: string;
  png: ArrayBuffer;
  items: HostMenuItem[];
}

export interface HostPerfHud {
  winId: number;
  visible: boolean;
  live: boolean;
  headerLine: string;
  pacingLine: string;
  stageLine: string;
  inputLine: string;
  dtMs: number[];
  uiGlMs: number[];
  gpuMs: number[];
  copySwapMs: number[];
}

export interface HostBridgeCallbacks {
  onWinCreate: (cfg: HostWinConfig) => void;
  onWinConfig: (cfg: HostWinConfig) => void;
  onWinAction: (winId: number, action: number) => void;
  onFirstFrame: (winId: number) => void;
  onPerfHud: (hud: HostPerfHud) => void;
  onPopupMenu: (reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]) => void;
  onClipWrite: (data: HostClipWrite) => void;
  onClipRead: (reqId: number, formats: string[]) => void;
  onDialogOpen: (dlg: HostFileDialog) => void;
  onDialogSave: (dlg: HostFileDialog) => void;
  onDialogMsg: (box: HostMessageBox) => void;
  onShellReq: (reqId: number, op: number, target: string) => void;
  onNowPlaying: (np: HostNowPlaying) => void;
  onNotifShow: (notif: HostNotifShow) => void;
  onNotifRemove: (all: boolean, id: string) => void;
  onPowerReq: (req: HostPowerReq) => void;
  onNetWatch: () => void;
  onCookieReq: (req: HostCookieReq) => void;
  onWebReq: (winId: number, op: number, arg1: string, arg2: string) => void;
  onWebSetup: (setup: HostWebSetup) => void;
  onPlaceWebViews: (place: HostPlaceWebViews) => void;
  onWebAsyncReq: (req: HostWebAsyncReq) => void;
  onSchemeRes: (res: HostSchemeRes) => void;
  onTrayUpdate: (tray: HostTrayUpdate) => void;
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
    displays: HostDisplayInfo[],
    textScale?: number
  ): void;
  bindWindow(
    winId: number,
    uiContext: UIContext,
    content: NodeContent,
    arkWindowId: number,
    scale: number,
    refreshRate: number,
    darkMode: boolean,
    bgARGB?: number
  ): void;
  unbindWindow(winId: number): void;
  notifyWindowEvent(winId: number, op: number): void;
  notifyWindowRect(winId: number, xDip: number, yDip: number, wDip: number, hDip: number): void;
  notifyTitleBar(winId: number, heightDip: number, leftDip: number, rightDip: number): void;
  resolvePopupMenu(reqId: number, chosenId: number): void;
  notifyClipChanged(changeCount: number, external: boolean, mimes: string[]): void;
  resolveClipRead(reqId: number, err: string, reps: HostClipRep[]): void;
  resolveDialog(reqId: number, response: number, checkboxChecked: boolean, err: string, paths: string[]): void;
  resolveShell(reqId: number, err: string): void;
  notifyAppOpen(op: number, items: string[]): void;
  notifyMediaCmd(cmd: number, posMs: number): void;
  resolveNotif(reqId: number, denied: boolean, err: string): void;
  notifyNotifClicked(id: string): void;
  notifyPowerState(onBattery: boolean, event: string): void;
  notifyNetState(
    online: boolean,
    metered: boolean,
    proxyHost?: string,
    proxyPort?: number,
    proxyUser?: string,
    proxyPass?: string,
    proxyExclusions?: string[]
  ): void;
  resolveCookie(reqId: number, err: string, cookies: HostCookie[]): void;
  notifyWebEvent(
    winId: number,
    ev: number,
    canBack: boolean,
    canForward: boolean,
    errCode: number,
    text: string,
    desc: string
  ): void;
  resolveWebAsync(reqId: number, text: string, err: string, data: ArrayBuffer): void;
  notifySchemeReq(
    reqId: number,
    webId: number,
    cancel: boolean,
    method: string,
    url: string,
    headers: HostSchemeHeader[],
    body: ArrayBuffer
  ): void;
  clearWebControllerHandler(handler: Object): void;
  notifyTrayEvent(trayId: number, ev: number, menuItemId: number): void;
  setDarkMode(dark: boolean): void;
  crash(): void;
}

declare const host: HostModule;
export default host;
