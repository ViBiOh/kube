package log

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/ViBiOh/kmux/pkg/client"
	"github.com/ViBiOh/kmux/pkg/concurrent"
	"github.com/ViBiOh/kmux/pkg/output"
	"github.com/ViBiOh/kmux/pkg/resource"
	"github.com/fatih/color"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type Logger struct {
	selector        map[string]string
	containerRegexp *regexp.Regexp
	logRegexp       *regexp.Regexp
	colorFilter     *color.Color
	resourceType    string
	resourceName    string
	jsonColorKeys   []string
	since           int64
	rawOutput       bool
	dryRun          bool
	invertRegexp    bool
}

func NewLogger(resourceType, resourceName string, selector map[string]string, since time.Duration) Logger {
	return Logger{
		resourceType: resourceType,
		resourceName: resourceName,
		selector:     selector,
		since:        int64(since.Seconds()),
	}
}

func (l Logger) WithDryRun(dryRun bool) Logger {
	l.dryRun = dryRun

	return l
}

func (l Logger) WithContainerRegexp(containerRegexp *regexp.Regexp) Logger {
	l.containerRegexp = containerRegexp

	return l
}

func (l Logger) WithLogRegexp(logRegexp *regexp.Regexp) Logger {
	l.logRegexp = logRegexp

	return l
}

func (l Logger) WithInvertRegexp(invertRegexp bool) Logger {
	l.invertRegexp = invertRegexp

	return l
}

func (l Logger) WithColorFilter(colorFilter *color.Color) Logger {
	l.colorFilter = colorFilter

	return l
}

func (l Logger) WithJsonColorKeys(jsonColorKeys []string) Logger {
	l.jsonColorKeys = jsonColorKeys

	return l
}

func (l Logger) WithRawOutput(rawOutput bool) Logger {
	l.rawOutput = rawOutput

	return l
}

func (l Logger) Log(ctx context.Context, kube client.Kube) error {
	podWatcher, err := resource.WatchPods(ctx, kube, l.resourceType, l.resourceName, l.selector, l.dryRun)
	if err != nil {
		return fmt.Errorf("watch pods: %w", err)
	}

	defer podWatcher.Stop()

	var activeStreams sync.Map

	streaming := concurrent.NewSimple()

	for event := range podWatcher.ResultChan() {
		pod, ok := event.Object.(*v1.Pod)
		if !ok {
			continue
		}

		streamCancel, ok := activeStreams.Load(pod.UID)

		if event.Type == watch.Deleted || event.Type == watch.Error || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			if ok {
				streamCancel.(context.CancelFunc)()
				activeStreams.Delete(pod.UID)
			} else if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
				l.handlePod(ctx, kube, &activeStreams, streaming, *pod)
			}

			continue
		}

		if ok || pod.Status.Phase == v1.PodPending {
			continue
		}

		l.handlePod(ctx, kube, &activeStreams, streaming, *pod)
	}

	streaming.Wait()

	return nil
}

func (l Logger) handlePod(ctx context.Context, kube client.Kube, activeStreams *sync.Map, streaming *concurrent.Simple, pod v1.Pod) {
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if !l.isContainerSelected(container) {
			continue
		}

		container := container

		if l.dryRun {
			kube.Info("%s %s", output.Green.Sprintf("[%s/%s]", pod.Name, container.Name), output.Yellow.Sprint("Found!"))
			continue
		}

		streaming.Go(func() {
			if pod.Status.Phase != v1.PodRunning {
				l.logPod(ctx, kube, pod.Namespace, pod.Name, container.Name)
				return
			}

			streamCtx, streamCancel := context.WithCancel(ctx)
			activeStreams.Store(pod.UID, streamCancel)
			defer streamCancel()

			l.streamPod(streamCtx, kube, pod.Namespace, pod.Name, container.Name)
		})
	}
}

func (l Logger) isContainerSelected(container v1.Container) bool {
	if l.containerRegexp == nil {
		return true
	}

	return l.containerRegexp.MatchString(container.Name)
}

func (l Logger) logPod(ctx context.Context, kube client.Kube, namespace, name, container string) {
	content, err := kube.CoreV1().Pods(namespace).GetLogs(name, &v1.PodLogOptions{
		SinceSeconds: &l.since,
		Container:    container,
	}).DoRaw(ctx)
	if err != nil {
		kube.Err("%s", err)
		return
	}

	l.outputLog(kube, bytes.NewReader(content), l.logOutputter(kube, name, container))
}

func (l Logger) streamPod(ctx context.Context, kube client.Kube, namespace, name, container string) {
	stream, err := kube.CoreV1().Pods(namespace).GetLogs(name, &v1.PodLogOptions{
		Follow:       true,
		SinceSeconds: &l.since,
		Container:    container,
	}).Stream(ctx)
	if err != nil {
		kube.Err("%s", err)
		return
	}

	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			kube.Err("close stream: %s", closeErr)
		}
	}()

	l.outputLog(kube, stream, l.logOutputter(kube, name, container))
}

func (l Logger) logOutputter(kube client.Kube, name, container string) output.Outputter {
	return kube.Child(l.rawOutput, output.Green.Sprintf("[%s/%s]", name, container))
}

func (l Logger) outputLog(kube client.Kube, reader io.Reader, outputter output.Outputter) {
	if !l.rawOutput {
		outputter.Info(output.Yellow.Sprint("Log..."))
		defer outputter.Info(output.Yellow.Sprint("Log ended."))
	}

	streamScanner := bufio.NewScanner(reader)
	streamScanner.Split(bufio.ScanLines)

	var colorOutputter *color.Color

	for streamScanner.Scan() {
		text := streamScanner.Text()

		colorOutputter = ColorOfJSON(text, l.jsonColorKeys...)

		if colorIsGreater(colorOutputter, l.colorFilter) {
			continue
		}

		if l.logRegexp == nil {
			outputter.Std(Format(text, colorOutputter))

			continue
		}

		if match := l.logRegexp.MatchString(text); (match && l.invertRegexp) || (!l.invertRegexp && !match) {
			continue
		}

		outputter.Std(FormatGrep(text, l.logRegexp, colorOutputter))
	}
}
