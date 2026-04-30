import json
import os
import subprocess
import boto3
import logging
import shlex
import time

# Configure structured JSON logging
class JsonFormatter(logging.Formatter):
    def format(self, record):
        log_record = {
            "level": record.levelname.lower(),
            "time": int(time.time()),
            "message": record.getMessage(),
            "component": "task-executor",
        }
        # Add correlation IDs if present
        if hasattr(record, "task_id"):
            log_record["task_id"] = record.task_id
        if hasattr(record, "request_id"):
            log_record["request_id"] = record.request_id
        if hasattr(record, "chat_id"):
            log_record["chat_id"] = record.chat_id
        if record.exc_info:
            log_record["exception"] = self.formatException(record.exc_info)
        return json.dumps(log_record)

logger = logging.getLogger("executor")
handler = logging.StreamHandler()
handler.setFormatter(JsonFormatter())
logger.addHandler(handler)
logger.setLevel(logging.INFO)

sqs = boto3.client('sqs')
QUEUE_URL = os.environ.get('QUEUE_URL')
RESULT_QUEUE_URL = os.environ.get('RESULT_QUEUE_URL')

def http_handler(event, context):
    """Triggered by Function URL for immediate task submission."""
    req_id = context.aws_request_id
    extra = {"request_id": req_id}
    
    try:
        body = json.loads(event.get('body', '{}'))
        skill = body.get('skill')
        engine = body.get('engine')
        command = body.get('command')
        chat_id = body.get('chat_id')
        metadata = body.get('metadata', {})
        
        extra["chat_id"] = chat_id
        
        if not all([skill, engine, command, chat_id]):
            logger.error("Missing mandatory fields in request", extra=extra)
            return {'statusCode': 400, 'body': json.dumps({'error': 'Missing fields'})}

        logger.info(f"Received submission for skill '{skill}' ({engine}): {command}", extra=extra)

        # 1. Prepare environment
        task_root = os.environ.get('LAMBDA_TASK_ROOT', '/var/task')
        engine_script = os.path.join(task_root, engine)
        
        if not os.path.exists(engine_script):
            logger.error(f"Engine script not found: {engine}", extra=extra)
            return {'statusCode': 404, 'body': json.dumps({'error': f"Engine script not found: {engine}"})}

        env = os.environ.copy()
        env['PYTHONPATH'] = f"{task_root}/lib:" + env.get('PYTHONPATH', '')
        
        # 2. Run the submission command in a temporary directory (stateless)
        import tempfile
        with tempfile.TemporaryDirectory() as tmp_dir:
            args = shlex.split(command)
            process = subprocess.run(["python3", engine_script] + args, 
                                   cwd=tmp_dir, capture_output=True, text=True, env=env)
            
            result_str = process.stdout if process.returncode == 0 else process.stderr
            
            if process.returncode != 0:
                logger.error(f"Skill submission failed (code {process.returncode}): {process.stderr}", extra=extra)
                return {'statusCode': 500, 'body': json.dumps({'error': result_str})}

            # 3. Parse Task ID and queue for monitoring
            try:
                result_json = json.loads(result_str)
                task_id = result_json.get('id')
                if task_id:
                    extra["task_id"] = task_id
                    
                    # Capture any engine-provided metadata for persistence
                    engine_meta = result_json.get('_metadata', {})
                    if engine_meta:
                        metadata.update(engine_meta)

                    if QUEUE_URL:
                        monitor_msg = {
                            "type": "monitor_task",
                            "chat_id": chat_id,
                            "skill": skill,
                            "engine": engine,
                            "task_id": task_id,
                            "metadata": metadata,
                            "original_result": result_json
                        }
                        logger.info(f"Task accepted. Queuing monitor message for task_id: {task_id}", extra=extra)
                        sqs.send_message(QueueUrl=QUEUE_URL, MessageBody=json.dumps(monitor_msg), MessageGroupId=str(chat_id))
                else:
                    logger.warning("Submission succeeded but no task ID found in output", extra=extra)
            except Exception as e:
                logger.warning(f"Failed to parse submission result JSON: {e}", extra=extra)

            return {'statusCode': 200, 'body': result_str}

    except Exception as e:
        logger.error(f"HTTP Handler Error: {e}", extra=extra, exc_info=True)
        return {'statusCode': 500, 'body': json.dumps({'error': str(e)})}

def sqs_handler(event, context):
    """Triggered by TaskQueue to wait for task completion."""
    req_id = context.aws_request_id
    
    for record in event.get('Records', []):
        try:
            msg = json.loads(record.get('body', '{}'))
            if msg.get('type') != 'monitor_task': continue
            
            skill = msg.get('skill')
            engine = msg.get('engine')
            task_id = msg.get('task_id')
            chat_id = msg.get('chat_id')
            metadata = msg.get('metadata', {})
            
            # Extract SQS retry count
            attr = record.get('attributes', {})
            retry_count = attr.get('ApproximateReceiveCount', '1')
            
            extra = {"task_id": task_id, "request_id": req_id, "chat_id": chat_id}
            logger.info(f"Checking task status (Attempt {retry_count})", extra=extra)
            
            task_root = os.environ.get('LAMBDA_TASK_ROOT', '/var/task')
            engine_script = os.path.join(task_root, engine)
            
            env = os.environ.copy()
            env['PYTHONPATH'] = f"{task_root}/lib:" + env.get('PYTHONPATH', '')
            
            # Polling check in a temporary directory
            import tempfile
            with tempfile.TemporaryDirectory() as tmp_dir:
                check_process = subprocess.run(["python3", engine_script, "check", "--id", task_id], 
                                            cwd=tmp_dir, capture_output=True, text=True, env=env)
                
                try:
                    check_res = json.loads(check_process.stdout)
                    done = check_res.get('done') or check_res.get('finished') == 1
                    faulted = check_res.get('faulted') or check_res.get('error')
                    
                    if done or faulted:
                        logger.info(f"Task completed (success={done}, error={bool(faulted)}). Sending result to worker.", extra=extra)
                        if RESULT_QUEUE_URL:
                            result_msg = {
                                "type": "task_result",
                                "task_id": task_id,
                                "chat_id": chat_id,
                                "skill": skill,
                                "metadata": metadata,
                                "result": check_process.stdout,
                                "is_error": bool(faulted)
                            }
                            sqs.send_message(QueueUrl=RESULT_QUEUE_URL, MessageBody=json.dumps(result_msg), MessageGroupId=str(chat_id))
                        else:
                            logger.critical(f"RESULT_QUEUE_URL not configured. Cannot send result for task {task_id}", extra=extra)
                        return
                except Exception as e:
                    logger.error(f"Failed to parse engine check output: {check_process.stdout}", extra=extra)
                
                # Still in progress -> retry via Visibility Timeout
                raise Exception(f"Task {task_id} not finished yet")
        
        except Exception as e:
            if "not finished yet" in str(e):
                pass
            else:
                logger.error(f"SQS Handler Error: {e}", extra=extra, exc_info=True)
            raise e
