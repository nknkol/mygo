import host from 'libmygo_host.so';
import { AbilityConstant, Configuration, ConfigurationConstant, UIAbility, Want } from '@kit.AbilityKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { window } from '@kit.ArkUI';
import { hostBridge } from '../bridge/HostBridge';

const DOMAIN: number = 0x0000;
const TAG: string = 'mygo';

function isDarkMode(mode: ConfigurationConstant.ColorMode | undefined): boolean {
  return mode === ConfigurationConstant.ColorMode.COLOR_MODE_DARK;
}

export default class EntryAbility extends UIAbility {
  onCreate(want: Want, _launchParam: AbilityConstant.LaunchParam): void {
    hilog.info(DOMAIN, TAG, 'onCreate uri=%{public}s', want.uri ?? '');
    AppStorage.setOrCreate<boolean>('darkMode', isDarkMode(this.context.config?.colorMode));
    AppStorage.setOrCreate<number>('fontSizeScale', this.context.config?.fontSizeScale ?? 1.0);
    hostBridge.init(this.context);
    hostBridge.handleWant(want, false);
  }

  onNewWant(want: Want, _launchParam: AbilityConstant.LaunchParam): void {
    hilog.info(DOMAIN, TAG, 'onNewWant uri=%{public}s', want.uri ?? '');
    hostBridge.handleWant(want, true);
  }

  onConfigurationUpdate(newConfig: Configuration): void {
    const dark: boolean = isDarkMode(newConfig.colorMode);
    AppStorage.setOrCreate<boolean>('darkMode', dark);
    if (newConfig.fontSizeScale !== undefined && newConfig.fontSizeScale > 0) {
      AppStorage.setOrCreate<number>('fontSizeScale', newConfig.fontSizeScale);
    }
    host.setDarkMode(dark);
    hostBridge.refreshEnvAndDisplays();
  }

  onWindowStageCreate(windowStage: window.WindowStage): void {
    try {
      const mainWindow: window.Window = windowStage.getMainWindowSync();
      const props: window.WindowProperties = mainWindow.getWindowProperties();
      AppStorage.setOrCreate<number>('windowId', props.id);
      hostBridge.attachMainStage(windowStage, mainWindow);
    } catch (err) {
      hilog.error(DOMAIN, TAG, 'getMainWindowSync failed: %{public}s', JSON.stringify(err));
    }

    windowStage.loadContent('pages/Index', (err) => {
      if (err.code) {
        hilog.error(DOMAIN, TAG, 'loadContent failed: %{public}s', JSON.stringify(err));
        return;
      }
      hostBridge.onMainContentLoaded();
    });
  }

  onWindowStageDestroy(): void {
    hilog.info(DOMAIN, TAG, 'onWindowStageDestroy');
    hostBridge.destroyStage();
  }

  onForeground(): void {
    hostBridge.onAppForeground();
  }

  onBackground(): void {
    hostBridge.onAppBackground();
  }

  onDestroy(): void {
    hilog.info(DOMAIN, TAG, 'onDestroy');
  }
}
